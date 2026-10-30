package service

import (
	"context"
	"errors"
	"math"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeRequestSample 是 request_metrics 的一行测试样本。
type fakeRequestSample struct {
	groupID             int64
	at                  time.Time
	statusCode          int
	upstreamError       bool
	firstTokenMS        int
	inputTokens         int
	cacheReadTokens     int
	cacheCreationTokens int
}

type fakeGroupMetricKey struct {
	groupID int64
	hour    time.Time
}

// fakeGroupMetricRepo 是聚合仓库的内存替身。
//
// AggregateRequestMetrics 按与生产 SQL 相同的口径从样本汇总；Health 直接读样本，
// 保证服务层测试覆盖真实的阈值算术而不是常量回放。
type fakeGroupMetricRepo struct {
	samples []fakeRequestSample
	metrics map[fakeGroupMetricKey]GroupMetric

	aggregateErr   error
	listErr        error
	healthErr      error
	latestHourErr  error
	aggregateCalls []time.Time
	healthCalls    int

	lastAggregateFrom time.Time
	lastAggregateTo   time.Time
	lastUpsertHour    time.Time
	upsertCalls       int
}

func newFakeGroupMetricRepo() *fakeGroupMetricRepo {
	return &fakeGroupMetricRepo{metrics: make(map[fakeGroupMetricKey]GroupMetric)}
}

func (r *fakeGroupMetricRepo) addSample(s fakeRequestSample) {
	r.samples = append(r.samples, s)
}

func (r *fakeGroupMetricRepo) AggregateRequestMetrics(
	_ context.Context,
	from, to time.Time,
) ([]GroupMetric, error) {
	r.lastAggregateFrom = from
	r.lastAggregateTo = to
	r.aggregateCalls = append(r.aggregateCalls, from)
	if r.aggregateErr != nil {
		return nil, r.aggregateErr
	}

	byGroup := make(map[int64][]fakeRequestSample)
	for _, sample := range r.samples {
		if sample.at.Before(from) || !sample.at.Before(to) {
			continue
		}
		byGroup[sample.groupID] = append(byGroup[sample.groupID], sample)
	}

	ids := make([]int64, 0, len(byGroup))
	for id := range byGroup {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	out := make([]GroupMetric, 0, len(ids))
	for _, id := range ids {
		out = append(out, aggregateFakeSamples(id, from, byGroup[id]))
	}
	return out, nil
}

func aggregateFakeSamples(groupID int64, hour time.Time, samples []fakeRequestSample) GroupMetric {
	m := GroupMetric{GroupID: groupID, Hour: hour.UTC(), Requests: len(samples)}
	ttft := make([]int, 0, len(samples))
	for _, sample := range samples {
		if sample.upstreamError {
			m.UpstreamFailures++
		} else if sample.statusCode >= 400 && sample.statusCode < 500 {
			m.RequestErrors++
		}
		if sample.firstTokenMS > 0 {
			ttft = append(ttft, sample.firstTokenMS)
		}
		m.InputTokens += int64(sample.inputTokens)
		m.CacheReadTokens += int64(sample.cacheReadTokens)
		m.CacheCreationTokens += int64(sample.cacheCreationTokens)
	}
	m.TTFTSamples = len(ttft)
	if len(ttft) > 0 {
		sort.Ints(ttft)
		m.P50TTFT = fakePercentile(ttft, 0.5)
		m.P95TTFT = fakePercentile(ttft, 0.95)
	}
	denominator := m.InputTokens + m.CacheReadTokens + m.CacheCreationTokens
	if denominator > 0 {
		m.CacheHitRate = float64(m.CacheReadTokens) / float64(denominator)
	}
	return m
}

// fakePercentile 复刻 percentile_cont 的线性插值。
func fakePercentile(sorted []int, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if len(sorted) == 1 {
		return float64(sorted[0])
	}
	pos := p * float64(len(sorted)-1)
	lower := int(math.Floor(pos))
	upper := int(math.Ceil(pos))
	if lower == upper {
		return float64(sorted[lower])
	}
	frac := pos - float64(lower)
	return float64(sorted[lower]) + frac*float64(sorted[upper]-sorted[lower])
}

func (r *fakeGroupMetricRepo) UpsertGroupMetrics(
	_ context.Context,
	hour time.Time,
	metrics []GroupMetric,
) error {
	r.upsertCalls++
	r.lastUpsertHour = hour
	for _, m := range metrics {
		m.Hour = hour.UTC()
		r.metrics[fakeGroupMetricKey{groupID: m.GroupID, hour: hour.UTC()}] = m
	}
	return nil
}

func (r *fakeGroupMetricRepo) ListGroupMetrics(
	_ context.Context,
	groupIDs []int64,
	from, to time.Time,
) ([]GroupMetric, error) {
	if r.listErr != nil {
		return nil, r.listErr
	}
	allowed := make(map[int64]struct{}, len(groupIDs))
	for _, id := range groupIDs {
		allowed[id] = struct{}{}
	}
	out := make([]GroupMetric, 0)
	for key, m := range r.metrics {
		if _, ok := allowed[key.groupID]; !ok {
			continue
		}
		if m.Hour.Before(from) || !m.Hour.Before(to) {
			continue
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].GroupID != out[j].GroupID {
			return out[i].GroupID < out[j].GroupID
		}
		return out[i].Hour.Before(out[j].Hour)
	})
	return out, nil
}

func (r *fakeGroupMetricRepo) GroupHealthSamples(
	_ context.Context,
	groupIDs []int64,
	since, recentSince time.Time,
) (map[int64]GroupHealthSample, error) {
	r.healthCalls++
	if r.healthErr != nil {
		return nil, r.healthErr
	}
	out := make(map[int64]GroupHealthSample, len(groupIDs))
	for _, groupID := range groupIDs {
		var window GroupHealthSample
		var ttft []int
		for _, sample := range r.samples {
			if sample.groupID != groupID || sample.at.Before(since) {
				continue
			}
			window.Requests++
			if sample.upstreamError {
				window.UpstreamFailures++
			}
			if sample.at.After(recentSince) || sample.at.Equal(recentSince) {
				window.RecentRequests++
				if !sample.upstreamError {
					window.RecentSuccesses++
				}
			}
			if sample.firstTokenMS > 0 {
				ttft = append(ttft, sample.firstTokenMS)
			}
		}
		sort.Ints(ttft)
		if len(ttft) > 0 {
			window.P95TTFT = fakePercentile(ttft, 0.95)
		}
		out[groupID] = window
	}
	return out, nil
}

func (r *fakeGroupMetricRepo) LatestGroupMetricHour(context.Context) (*time.Time, error) {
	if r.latestHourErr != nil {
		return nil, r.latestHourErr
	}
	var latest *time.Time
	for key := range r.metrics {
		hour := key.hour.UTC().Truncate(time.Hour)
		if latest == nil || hour.After(*latest) {
			latest = &hour
		}
	}
	return latest, nil
}

func (r *fakeGroupMetricRepo) getMetric(t *testing.T, groupID int64, hour time.Time) GroupMetric {
	t.Helper()
	m, ok := r.metrics[fakeGroupMetricKey{groupID: groupID, hour: hour.UTC()}]
	require.True(t, ok, "missing metric for group %d hour %s", groupID, hour)
	return m
}

func newTestMetricsAggregator(repo GroupMetricRepository, now time.Time) *MetricsAggregator {
	agg := NewMetricsAggregator(repo)
	agg.now = func() time.Time { return now }
	return agg
}

func TestAggregateHourComputesFailureRateAndCacheHit(t *testing.T) {
	const groupID = int64(42)
	hour := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
	repo := newFakeGroupMetricRepo()
	for i := 0; i < 20; i++ {
		repo.addSample(fakeRequestSample{
			groupID:             groupID,
			at:                  hour.Add(time.Duration(i)*time.Minute + 7*time.Minute),
			statusCode:          200,
			upstreamError:       i < 2,
			inputTokens:         5,
			cacheReadTokens:     40,
			cacheCreationTokens: 5,
		})
	}
	// 前两条是 502 上游失败；其余 18 条成功。
	for i := 0; i < 2; i++ {
		repo.samples[i].statusCode = 502
	}

	agg := newTestMetricsAggregator(repo, hour.Add(90*time.Minute))
	// 传入非整点时间也必须落到整点桶。
	require.NoError(t, agg.AggregateHour(context.Background(), hour.Add(17*time.Minute)))

	m := repo.getMetric(t, groupID, hour)
	require.Equal(t, 20, m.Requests)
	require.Equal(t, 2, m.UpstreamFailures)
	require.InDelta(t, 0.8, m.CacheHitRate, 1e-9)
	require.True(t, repo.lastAggregateFrom.Equal(hour))
	require.True(t, repo.lastAggregateTo.Equal(hour.Add(time.Hour)))

	// 可重算：同一小时再跑一次仍然只有一行，数值不变。
	repo.addSample(fakeRequestSample{
		groupID:             groupID,
		at:                  hour.Add(59 * time.Minute),
		statusCode:          200,
		inputTokens:         1000,
		cacheReadTokens:     0,
		cacheCreationTokens: 0,
	})
	require.NoError(t, agg.AggregateHour(context.Background(), hour))
	m = repo.getMetric(t, groupID, hour)
	require.Equal(t, 21, m.Requests)
	require.Equal(t, 2, m.UpstreamFailures)
	require.InDelta(t, 800.0/2000.0, m.CacheHitRate, 1e-9)
	require.Len(t, repo.metrics, 1)
}

func TestAggregateHourSeparatesUserErrorsFromUpstreamFailures(t *testing.T) {
	const groupID = int64(7)
	hour := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
	repo := newFakeGroupMetricRepo()
	statuses := []struct {
		code     int
		upstream bool
	}{
		{200, false}, {200, false}, {200, false}, {200, false},
		{200, false}, {200, false}, {200, false}, {429, false},
		{500, true}, {0, true},
	}
	for i, s := range statuses {
		repo.addSample(fakeRequestSample{
			groupID:       groupID,
			at:            hour.Add(time.Duration(i) * time.Minute),
			statusCode:    s.code,
			upstreamError: s.upstream,
		})
	}

	agg := newTestMetricsAggregator(repo, hour.Add(time.Hour))
	require.NoError(t, agg.AggregateHour(context.Background(), hour))

	m := repo.getMetric(t, groupID, hour)
	require.Equal(t, 10, m.Requests)
	require.Equal(t, 2, m.UpstreamFailures, "5xx and status=0 are upstream failures")
	require.Equal(t, 1, m.RequestErrors, "429 is a user error, not an upstream failure")
}

func TestAggregateHourRejectsZeroHour(t *testing.T) {
	agg := newTestMetricsAggregator(newFakeGroupMetricRepo(), time.Now())
	require.Error(t, agg.AggregateHour(context.Background(), time.Time{}))
}

func TestAggregateHourPropagatesRepositoryError(t *testing.T) {
	repo := newFakeGroupMetricRepo()
	repo.aggregateErr = errors.New("db down")
	agg := newTestMetricsAggregator(repo, time.Now())
	require.ErrorIs(t, agg.AggregateHour(context.Background(), time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)), repo.aggregateErr)
}

// TestHealthMarksDegradedOnlyWithEnoughSamples 是简报里的硬性用例：
// 5 条请求、3 条失败，样本不足 20，不得因为失败率而降级。
func TestHealthMarksDegradedOnlyWithEnoughSamples(t *testing.T) {
	const groupID = int64(9)
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	repo := newFakeGroupMetricRepo()
	for i := 0; i < 5; i++ {
		repo.addSample(fakeRequestSample{
			groupID:       groupID,
			at:            now.Add(-5 * time.Minute),
			statusCode:    200,
			upstreamError: i < 3,
		})
	}
	// 两条成功的请求必须在最近 15 分钟内，否则会命中“15 分钟无成功”判定。
	repo.samples[3].upstreamError = false
	repo.samples[4].upstreamError = false

	agg := newTestMetricsAggregator(repo, now)
	h, err := agg.Health(context.Background(), groupID)
	require.NoError(t, err)
	require.False(t, h.Degraded, "must not degrade below 20 samples")
	require.Equal(t, 5, h.SampleSize)
	require.Empty(t, h.Reason)
}

func TestHealthDegradesOnUpstreamFailureRate(t *testing.T) {
	const groupID = int64(10)
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	repo := newFakeGroupMetricRepo()
	for i := 0; i < 20; i++ {
		repo.addSample(fakeRequestSample{
			groupID:       groupID,
			at:            now.Add(-time.Duration(i) * time.Minute),
			statusCode:    200,
			upstreamError: i < 3,
		})
	}

	agg := newTestMetricsAggregator(repo, now)
	h, err := agg.Health(context.Background(), groupID)
	require.NoError(t, err)
	require.True(t, h.Degraded)
	require.Equal(t, DegradedReasonUpstreamFailureRate, h.Reason)
	require.Equal(t, 20, h.SampleSize)
}

func TestHealthDoesNotDegradeAtExactlyTenPercentFailures(t *testing.T) {
	const groupID = int64(11)
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	repo := newFakeGroupMetricRepo()
	for i := 0; i < 20; i++ {
		repo.addSample(fakeRequestSample{
			groupID:       groupID,
			at:            now.Add(-time.Duration(i) * time.Minute),
			statusCode:    200,
			upstreamError: i < 2,
		})
	}

	agg := newTestMetricsAggregator(repo, now)
	h, err := agg.Health(context.Background(), groupID)
	require.NoError(t, err)
	require.False(t, h.Degraded, "10% is the boundary, not over it")
	require.Equal(t, 20, h.SampleSize)
}

func TestHealthDegradesOnSlowP95TTFT(t *testing.T) {
	const groupID = int64(12)
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	repo := newFakeGroupMetricRepo()
	for i := 0; i < 20; i++ {
		repo.addSample(fakeRequestSample{
			groupID:      groupID,
			at:           now.Add(-time.Duration(i) * time.Minute),
			statusCode:   200,
			firstTokenMS: 12_000,
		})
	}

	agg := newTestMetricsAggregator(repo, now)
	h, err := agg.Health(context.Background(), groupID)
	require.NoError(t, err)
	require.True(t, h.Degraded)
	require.Equal(t, DegradedReasonSlowTTFT, h.Reason)
}

func TestHealthIgnoresNonStreamingSamplesForTTFT(t *testing.T) {
	const groupID = int64(13)
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	repo := newFakeGroupMetricRepo()
	for i := 0; i < 20; i++ {
		repo.addSample(fakeRequestSample{
			groupID:      groupID,
			at:           now.Add(-time.Duration(i) * time.Minute),
			statusCode:   200,
			firstTokenMS: 0, // 非流式请求没有首包时间
		})
	}

	agg := newTestMetricsAggregator(repo, now)
	h, err := agg.Health(context.Background(), groupID)
	require.NoError(t, err)
	require.False(t, h.Degraded, "TTFT must only use samples with a first-token time")
}

func TestHealthDegradesWhenNoSuccessInLastFifteenMinutes(t *testing.T) {
	const groupID = int64(14)
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	repo := newFakeGroupMetricRepo()
	// 20 分钟前有一条成功，最近 15 分钟只有失败：这是中断信号，不受 20 条样本门槛限制。
	repo.addSample(fakeRequestSample{
		groupID:    groupID,
		at:         now.Add(-20 * time.Minute),
		statusCode: 200,
	})
	for i := 0; i < 4; i++ {
		repo.addSample(fakeRequestSample{
			groupID:       groupID,
			at:            now.Add(-time.Duration(i+1) * time.Minute),
			statusCode:    502,
			upstreamError: true,
		})
	}

	agg := newTestMetricsAggregator(repo, now)
	h, err := agg.Health(context.Background(), groupID)
	require.NoError(t, err)
	require.True(t, h.Degraded)
	require.Equal(t, DegradedReasonNoRecentSuccess, h.Reason)
	require.Equal(t, 5, h.SampleSize)
}

func TestHealthDoesNotDegradeAtNineteenSamples(t *testing.T) {
	const groupID = int64(15)
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	repo := newFakeGroupMetricRepo()
	for i := 0; i < 19; i++ {
		repo.addSample(fakeRequestSample{
			groupID:       groupID,
			at:            now.Add(-time.Duration(i) * time.Minute),
			statusCode:    200,
			upstreamError: i < 5,
		})
	}

	agg := newTestMetricsAggregator(repo, now)
	h, err := agg.Health(context.Background(), groupID)
	require.NoError(t, err)
	require.False(t, h.Degraded, "19 samples is still below the 20-sample floor")
	require.Equal(t, 19, h.SampleSize)
}

func TestHealthDoesNotDegradeAtExactlyTenSecondsTTFT(t *testing.T) {
	const groupID = int64(16)
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	repo := newFakeGroupMetricRepo()
	for i := 0; i < 20; i++ {
		repo.addSample(fakeRequestSample{
			groupID:      groupID,
			at:           now.Add(-time.Duration(i) * time.Minute),
			statusCode:   200,
			firstTokenMS: 10_000,
		})
	}

	agg := newTestMetricsAggregator(repo, now)
	h, err := agg.Health(context.Background(), groupID)
	require.NoError(t, err)
	require.False(t, h.Degraded, "10s exactly is the boundary, not over it")
}

func TestHealthManyReturnsPerGroup(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	repo := newFakeGroupMetricRepo()
	for i := 0; i < 20; i++ {
		repo.addSample(fakeRequestSample{
			groupID:       20,
			at:            now.Add(-time.Duration(i) * time.Minute),
			statusCode:    200,
			upstreamError: i < 4,
		})
		repo.addSample(fakeRequestSample{
			groupID:    21,
			at:         now.Add(-time.Duration(i) * time.Minute),
			statusCode: 200,
		})
	}

	agg := newTestMetricsAggregator(repo, now)
	healths, err := agg.HealthMany(context.Background(), []int64{20, 21, 999})
	require.NoError(t, err)
	require.Len(t, healths, 3)
	require.True(t, healths[20].Degraded)
	require.Equal(t, DegradedReasonUpstreamFailureRate, healths[20].Reason)
	require.False(t, healths[21].Degraded)
	require.Equal(t, 20, healths[21].SampleSize)
	require.Equal(t, 0, healths[999].SampleSize)
}

func TestHealthPropagatesRepositoryError(t *testing.T) {
	repo := newFakeGroupMetricRepo()
	repo.healthErr = errors.New("db down")
	agg := newTestMetricsAggregator(repo, time.Now())
	_, err := agg.Health(context.Background(), 1)
	require.ErrorIs(t, err, repo.healthErr)
}

func TestGroupMetricsComputesHourAndDayWindows(t *testing.T) {
	const groupID = int64(21)
	now := time.Date(2026, 10, 29, 12, 30, 0, 0, time.UTC)
	repo := newFakeGroupMetricRepo()

	latest := time.Date(2026, 10, 29, 11, 0, 0, 0, time.UTC)
	repo.metrics[fakeGroupMetricKey{groupID: groupID, hour: latest}] = GroupMetric{
		GroupID:             groupID,
		Hour:                latest,
		Requests:            10,
		UpstreamFailures:    1,
		RequestErrors:       2,
		TTFTSamples:         10,
		InputTokens:         100,
		CacheReadTokens:     800,
		CacheCreationTokens: 100,
		P50TTFT:             500,
		P95TTFT:             900,
		CacheHitRate:        0.8,
	}
	previous := latest.Add(-time.Hour)
	repo.metrics[fakeGroupMetricKey{groupID: groupID, hour: previous}] = GroupMetric{
		GroupID:          groupID,
		Hour:             previous,
		Requests:         60,
		UpstreamFailures: 5,
		TTFTSamples:      60,
		InputTokens:      1000,
		P50TTFT:          200,
		P95TTFT:          400,
	}
	// 超出 24h 窗口的旧行不能被计入。
	old := latest.Add(-48 * time.Hour)
	repo.metrics[fakeGroupMetricKey{groupID: groupID, hour: old}] = GroupMetric{
		GroupID:  groupID,
		Hour:     old,
		Requests: 1000,
	}

	agg := newTestMetricsAggregator(repo, now)
	summaries, err := agg.GroupMetrics(context.Background(), []int64{groupID, 999})
	require.NoError(t, err)
	require.Len(t, summaries, 1)

	summary, ok := summaries[groupID]
	require.True(t, ok)
	require.Equal(t, 70, summary.SampleSize)
	require.NotNil(t, summary.AsOf)
	require.True(t, summary.AsOf.Equal(latest.Add(time.Hour)))

	require.NotNil(t, summary.LastHour)
	require.Equal(t, 10, summary.LastHour.Requests)
	require.InDelta(t, 0.9, summary.LastHour.SuccessRate, 1e-9)
	require.InDelta(t, 0.2, summary.LastHour.RequestErrorRate, 1e-9)
	require.True(t, summary.LastHour.HasTTFT)
	require.InDelta(t, 500, summary.LastHour.P50TTFT, 1e-9)
	require.InDelta(t, 900, summary.LastHour.P95TTFT, 1e-9)

	require.NotNil(t, summary.Last24h)
	require.Equal(t, 70, summary.Last24h.Requests)
	require.InDelta(t, 64.0/70.0, summary.Last24h.SuccessRate, 1e-9)
	require.InDelta(t, 2.0/70.0, summary.Last24h.RequestErrorRate, 1e-9)
	require.True(t, summary.Last24h.HasCache)
	require.InDelta(t, 800.0/2000.0, summary.Last24h.CacheHitRate, 1e-9)
	// 小时分位数不能精确合并成 24h 分位数；跨桶时不得伪造。
	require.False(t, summary.Last24h.HasTTFT)
}

func TestGroupMetricsSingleBucketCanExposeTTFT(t *testing.T) {
	const groupID = int64(22)
	now := time.Date(2026, 10, 29, 12, 30, 0, 0, time.UTC)
	hour := time.Date(2026, 10, 29, 11, 0, 0, 0, time.UTC)
	repo := newFakeGroupMetricRepo()
	repo.metrics[fakeGroupMetricKey{groupID: groupID, hour: hour}] = GroupMetric{
		GroupID:         groupID,
		Hour:            hour,
		Requests:        50,
		TTFTSamples:     50,
		P50TTFT:         300,
		P95TTFT:         700,
		InputTokens:     10,
		CacheReadTokens: 90,
	}

	agg := newTestMetricsAggregator(repo, now)
	summaries, err := agg.GroupMetrics(context.Background(), []int64{groupID})
	require.NoError(t, err)
	require.True(t, summaries[groupID].Last24h.HasTTFT)
	require.InDelta(t, 700, summaries[groupID].Last24h.P95TTFT, 1e-9)
	require.InDelta(t, 0.9, summaries[groupID].Last24h.CacheHitRate, 1e-9)
}

func TestGroupMetricsSkipsIncompleteCurrentHour(t *testing.T) {
	const groupID = int64(23)
	now := time.Date(2026, 10, 29, 12, 30, 0, 0, time.UTC)
	currentHour := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	repo := newFakeGroupMetricRepo()
	repo.metrics[fakeGroupMetricKey{groupID: groupID, hour: currentHour}] = GroupMetric{
		GroupID:  groupID,
		Hour:     currentHour,
		Requests: 100,
	}

	agg := newTestMetricsAggregator(repo, now)
	summaries, err := agg.GroupMetrics(context.Background(), []int64{groupID})
	require.NoError(t, err)
	require.Empty(t, summaries, "a not-yet-finished hour must not be shown as final stats")
}

func TestGroupMetricsSkipsEmptyGroupList(t *testing.T) {
	repo := newFakeGroupMetricRepo()
	repo.listErr = errors.New("must not query")
	agg := newTestMetricsAggregator(repo, time.Now())
	summaries, err := agg.GroupMetrics(context.Background(), nil)
	require.NoError(t, err)
	require.Empty(t, summaries)
}

func TestGroupMetricsDropsStaleLastHourButKeepsDayWindow(t *testing.T) {
	const groupID = int64(24)
	now := time.Date(2026, 10, 29, 12, 30, 0, 0, time.UTC)
	staleHour := time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
	repo := newFakeGroupMetricRepo()
	repo.metrics[fakeGroupMetricKey{groupID: groupID, hour: staleHour}] = GroupMetric{
		GroupID:  groupID,
		Hour:     staleHour,
		Requests: 60,
	}

	agg := newTestMetricsAggregator(repo, now)
	summaries, err := agg.GroupMetrics(context.Background(), []int64{groupID})
	require.NoError(t, err)
	summary, ok := summaries[groupID]
	require.True(t, ok)

	require.Nil(t, summary.LastHour, "a bucket older than one hour must not masquerade as last_hour")
	require.NotNil(t, summary.Last24h)
	require.Equal(t, 60, summary.Last24h.Requests)
	require.Equal(t, 60, summary.SampleSize)
	require.NotNil(t, summary.AsOf)
	require.True(t, summary.AsOf.Equal(staleHour.Add(time.Hour)),
		"as_of describes the newest bucket in the window, even when stale")
}

func TestGroupMetricsKeepsWindowAnchoredAcrossMissingHours(t *testing.T) {
	const groupID = int64(25)
	now := time.Date(2026, 10, 29, 12, 30, 0, 0, time.UTC)
	currentHour := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	repo := newFakeGroupMetricRepo()
	// 09:00 与 11:00 有数据，10:00 缺失：窗口仍应固定为当前小时，不向最新桶收缩。
	repo.metrics[fakeGroupMetricKey{groupID: groupID, hour: currentHour.Add(-3 * time.Hour)}] = GroupMetric{
		GroupID:  groupID,
		Hour:     currentHour.Add(-3 * time.Hour),
		Requests: 40,
	}
	repo.metrics[fakeGroupMetricKey{groupID: groupID, hour: currentHour.Add(-time.Hour)}] = GroupMetric{
		GroupID:  groupID,
		Hour:     currentHour.Add(-time.Hour),
		Requests: 20,
	}

	agg := newTestMetricsAggregator(repo, now)
	summaries, err := agg.GroupMetrics(context.Background(), []int64{groupID})
	require.NoError(t, err)
	summary := summaries[groupID]

	require.NotNil(t, summary.Last24h)
	require.Equal(t, 60, summary.Last24h.Requests, "missing hours must not shift the 24h window")
	require.NotNil(t, summary.LastHour)
	require.Equal(t, 20, summary.LastHour.Requests)
	require.True(t, summary.AsOf.Equal(currentHour))
}

func TestMetricsAggregatorCatchesUpAfterDowntime(t *testing.T) {
	now := time.Date(2026, 10, 29, 15, 30, 0, 0, time.UTC)
	repo := newFakeGroupMetricRepo()
	lastAggregated := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	repo.metrics[fakeGroupMetricKey{groupID: 1, hour: lastAggregated}] = GroupMetric{
		GroupID:  1,
		Hour:     lastAggregated,
		Requests: 1,
	}

	agg := newTestMetricsAggregator(repo, now)
	agg.runOnce()

	require.Equal(t, []time.Time{
		time.Date(2026, 10, 29, 13, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 29, 14, 0, 0, 0, time.UTC),
	}, repo.aggregateCalls, "hours missed while the process was down must be backfilled in order")

	// 已经追平后再跑一次不应重复聚合。
	repo.aggregateCalls = nil
	agg.runOnce()
	require.Empty(t, repo.aggregateCalls)
}

func TestMetricsAggregatorRetriesFailedHourWithoutSkipping(t *testing.T) {
	current := time.Date(2026, 10, 29, 14, 30, 0, 0, time.UTC)
	repo := newFakeGroupMetricRepo()
	lastAggregated := time.Date(2026, 10, 29, 11, 0, 0, 0, time.UTC)
	repo.metrics[fakeGroupMetricKey{groupID: 1, hour: lastAggregated}] = GroupMetric{
		GroupID:  1,
		Hour:     lastAggregated,
		Requests: 1,
	}

	agg := newTestMetricsAggregator(repo, current)
	agg.now = func() time.Time { return current }
	repo.aggregateErr = errors.New("db down")
	agg.runOnce()
	require.Equal(t, []time.Time{time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)}, repo.aggregateCalls)

	repo.aggregateErr = nil
	repo.aggregateCalls = nil
	current = current.Add(time.Hour) // 15:30：上一完整小时是 14:00
	agg.runOnce()

	require.Equal(t, []time.Time{
		time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 29, 13, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 29, 14, 0, 0, 0, time.UTC),
	}, repo.aggregateCalls, "the failed hour must be retried before newer hours")
}

func TestMetricsAggregatorCapsBackfillAtLookback(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 30, 0, 0, time.UTC)
	currentHour := now.Truncate(time.Hour)
	repo := newFakeGroupMetricRepo()
	veryOld := currentHour.Add(-72 * time.Hour)
	repo.metrics[fakeGroupMetricKey{groupID: 1, hour: veryOld}] = GroupMetric{
		GroupID:  1,
		Hour:     veryOld,
		Requests: 1,
	}

	agg := newTestMetricsAggregator(repo, now)
	agg.runOnce()

	require.Len(t, repo.aggregateCalls, 48)
	require.True(t, repo.aggregateCalls[0].Equal(currentHour.Add(-MetricsAggregationLookback)))
	require.True(t, repo.aggregateCalls[len(repo.aggregateCalls)-1].Equal(currentHour.Add(-time.Hour)))
}

func TestHealthManySharesThirtySecondCache(t *testing.T) {
	const groupID = int64(26)
	current := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	repo := newFakeGroupMetricRepo()
	repo.addSample(fakeRequestSample{
		groupID:    groupID,
		at:         current.Add(-time.Minute),
		statusCode: 200,
	})

	agg := newTestMetricsAggregator(repo, current)
	agg.now = func() time.Time { return current }

	_, err := agg.HealthMany(context.Background(), []int64{groupID})
	require.NoError(t, err)
	_, err = agg.HealthMany(context.Background(), []int64{groupID})
	require.NoError(t, err)
	require.Equal(t, 1, repo.healthCalls, "a second shelf request within 30s must reuse the cache")

	current = current.Add(29 * time.Second)
	_, err = agg.HealthMany(context.Background(), []int64{groupID})
	require.NoError(t, err)
	require.Equal(t, 1, repo.healthCalls)

	current = current.Add(time.Second) // 距首次查询正好 30 秒，缓存过期
	_, err = agg.HealthMany(context.Background(), []int64{groupID})
	require.NoError(t, err)
	require.Equal(t, 2, repo.healthCalls)
}

func TestHealthManyDoesNotCacheFailures(t *testing.T) {
	const groupID = int64(27)
	repo := newFakeGroupMetricRepo()
	repo.healthErr = errors.New("db down")
	agg := newTestMetricsAggregator(repo, time.Now())

	_, err := agg.HealthMany(context.Background(), []int64{groupID})
	require.ErrorIs(t, err, repo.healthErr)

	repo.healthErr = nil
	healths, err := agg.HealthMany(context.Background(), []int64{groupID})
	require.NoError(t, err)
	require.Equal(t, 2, repo.healthCalls, "a failed lookup must not poison the cache")
	require.Contains(t, healths, groupID)
}

func TestNextAggregationRunIsAtMinuteFive(t *testing.T) {
	loc := time.UTC
	cases := []struct {
		now  time.Time
		want time.Time
	}{
		{time.Date(2026, 10, 29, 10, 0, 0, 0, loc), time.Date(2026, 10, 29, 10, 5, 0, 0, loc)},
		{time.Date(2026, 10, 29, 10, 4, 59, 0, loc), time.Date(2026, 10, 29, 10, 5, 0, 0, loc)},
		{time.Date(2026, 10, 29, 10, 5, 0, 0, loc), time.Date(2026, 10, 29, 11, 5, 0, 0, loc)},
		{time.Date(2026, 10, 29, 10, 6, 0, 0, loc), time.Date(2026, 10, 29, 11, 5, 0, 0, loc)},
		{time.Date(2026, 10, 29, 23, 59, 0, 0, loc), time.Date(2026, 10, 30, 0, 5, 0, 0, loc)},
	}
	for _, tc := range cases {
		require.Truef(t, nextAggregationRun(tc.now).Equal(tc.want),
			"now=%s want=%s got=%s", tc.now, tc.want, nextAggregationRun(tc.now))
	}
}

func TestMetricsAggregatorStartStopIsIdempotent(t *testing.T) {
	repo := newFakeGroupMetricRepo()
	now := time.Date(2026, 10, 29, 12, 30, 0, 0, time.UTC)
	agg := newTestMetricsAggregator(repo, now)
	agg.Start()
	agg.Stop()
	agg.Stop()

	// Start 会立即补算上一个完整小时。
	require.True(t, repo.lastAggregateFrom.Equal(time.Date(2026, 10, 29, 11, 0, 0, 0, time.UTC)))
	require.True(t, repo.lastAggregateTo.Equal(time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)))
}
