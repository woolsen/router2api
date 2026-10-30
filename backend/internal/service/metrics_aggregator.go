package service

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"
)

const (
	// DegradedMinSamples 是判定分组降级所需的最小小时样本数。
	// 低于该值只说明数据不足，不能据此给商户打降级标记。
	DegradedMinSamples = 20
	// DegradedMaxUpstreamFailureRate 是上游失败率阈值（严格大于才降级）。
	// 4xx 属于用户错误，不计入该分母之外的失败分子。
	DegradedMaxUpstreamFailureRate = 0.10
	// DegradedMaxP95TTFTMs 是首包 P95 阈值（毫秒，严格大于才降级）。
	DegradedMaxP95TTFTMs = 10_000.0
	// DegradedNoSuccessWindow 是“有请求但无成功”的中断判定窗口。
	DegradedNoSuccessWindow = 15 * time.Minute

	// MarketStatsMinSamples 是货架展示指标的最小 24h 样本数。
	// 与降级阈值 20 刻意分开：20 是运维信号，50 是对用户展示数字的承诺。
	MarketStatsMinSamples = 50
	// MetricsAggregationLookback 是补算的最大回看范围。
	// 停机超过该范围时只补最近 48 个整点，避免一次启动扫描无限历史。
	MetricsAggregationLookback = 48 * time.Hour
	// MarketHealthCacheTTL 是货架健康判定结果的共享缓存时间。
	// 每张货架卡片都看同一批分组，30 秒足够便宜且不会让降级标记明显滞后。
	MarketHealthCacheTTL = 30 * time.Second
)

// 降级原因使用稳定字符串，前端与运维告警可以据此区分信号。
const (
	// DegradedReasonNoRecentSuccess 表示最近 15 分钟有请求但没有任何非上游失败请求。
	DegradedReasonNoRecentSuccess = "no_success_15m"
	// DegradedReasonUpstreamFailureRate 表示上游失败率超过 10%。
	DegradedReasonUpstreamFailureRate = "upstream_failure_rate"
	// DegradedReasonSlowTTFT 表示流式首包 P95 超过 10s。
	DegradedReasonSlowTTFT = "slow_ttft"
)

var (
	// ErrMetricsHourRequired 表示聚合没有指定小时。
	ErrMetricsHourRequired = errors.New("metrics aggregation hour is required")
	// ErrMetricsAggregatorUnavailable 表示聚合器缺少仓库依赖。
	ErrMetricsAggregatorUnavailable = errors.New("metrics aggregator repository is not configured")
)

// GroupMetric 是某个分组在某个整点小时内的小时聚合结果。
//
// P50TTFT / P95TTFT 只统计 first_token_ms > 0 的流式样本；CacheHitRate 的
// 分母是 input + cache_read + cache_creation。请求数、失败数与 token 总量
// 一并保留，保证 24h 档可以精确求和，而不是把小时比率平均。
type GroupMetric struct {
	GroupID int64
	Hour    time.Time
	// Requests 是该小时进入网关的全部请求数。
	Requests int
	// UpstreamFailures 是归因于上游的失败数（5xx、未写出响应、带内流失败）。
	UpstreamFailures int
	// RequestErrors 是 4xx 用户错误数，单独展示，绝不并入上游失败。
	RequestErrors int
	// TTFTSamples 是有首包时间的流式样本数（分位数分母）。
	TTFTSamples int
	// InputTokens / CacheReadTokens / CacheCreationTokens 是求和原始值，
	// 24h 缓存命中率必须用总量重算，不能平均小时命中率。
	InputTokens         int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	P50TTFT             float64
	P95TTFT             float64
	CacheHitRate        float64
}

// GroupHealthSample 是 Health 判定所用的滚动窗口原始计数。
type GroupHealthSample struct {
	// Requests / UpstreamFailures 覆盖最近 1 小时。
	Requests         int
	UpstreamFailures int
	// P95TTFT 只覆盖最近 1 小时内有首包时间的流式样本。
	P95TTFT float64
	// RecentRequests / RecentSuccesses 覆盖最近 15 分钟。
	// success 指未被归因为上游失败（4xx 说明分组仍在服务，不算中断）。
	RecentRequests  int
	RecentSuccesses int
}

// Health 是分组的降级标记。
//
// Degraded 只用于展示与告警，绝不改变 Group.Status，也不下架分组。
type Health struct {
	Degraded   bool
	Reason     string
	SampleSize int
}

// GroupMetricWindow 是货架某一档窗口（最近 1 小时或最近 24 小时）的指标。
type GroupMetricWindow struct {
	Requests         int
	UpstreamFailures int
	RequestErrors    int
	TTFTSamples      int
	SuccessRate      float64
	RequestErrorRate float64
	// P50TTFT / P95TTFT 只有在窗口内恰好一个整点桶时才给出：
	// 小时分位数不能精确合并成多桶分位数，宁可不展示也不伪造。
	P50TTFT      float64
	P95TTFT      float64
	HasTTFT      bool
	CacheHitRate float64
	HasCache     bool
}

// GroupMetricSummary 是货架所需的 1h + 24h 两档指标。
type GroupMetricSummary struct {
	LastHour *GroupMetricWindow
	Last24h  *GroupMetricWindow
	// AsOf 是最新已聚合小时的截止时间（桶结束），不是请求时刻。
	AsOf       *time.Time
	SampleSize int
}

// GroupMetricRepository 是小时聚合与指标的持久化接口。
type GroupMetricRepository interface {
	// AggregateRequestMetrics 汇总 [from, to) 内的 request_metrics。
	AggregateRequestMetrics(ctx context.Context, from, to time.Time) ([]GroupMetric, error)
	// UpsertGroupMetrics 按 (group_id, hour) 幂等写入，重算覆盖旧值。
	UpsertGroupMetrics(ctx context.Context, hour time.Time, metrics []GroupMetric) error
	// ListGroupMetrics 读取 [from, to) 内指定分组的小时行。
	ListGroupMetrics(ctx context.Context, groupIDs []int64, from, to time.Time) ([]GroupMetric, error)
	// GroupHealthSamples 批量读取最近 1 小时与最近 15 分钟的原始计数。
	GroupHealthSamples(ctx context.Context, groupIDs []int64, since, recentSince time.Time) (map[int64]GroupHealthSample, error)
	// LatestGroupMetricHour 返回小时表中最大的已聚合小时；空表返回 nil。
	// 启动补算用它找回停机期间缺失的小时，而不是只看 now-1h。
	LatestGroupMetricHour(ctx context.Context) (*time.Time, error)
}

// cachedGroupHealth 是 HealthMany 的共享缓存条目。
type cachedGroupHealth struct {
	health    Health
	expiresAt time.Time
}

// MetricsAggregator 负责把 request_metrics 按整点聚合成 group_metric_hourly，
// 并提供分组健康判定与货架指标摘要。
type MetricsAggregator struct {
	repo GroupMetricRepository
	now  func() time.Time

	// aggregateMu 串行化补算；nextHour 是下一个待聚合的整点，零值表示尚未恢复。
	aggregateMu sync.Mutex
	nextHour    time.Time

	// healthMu 保护 30 秒共享缓存，避免每个货架请求都扫原始表。
	healthMu    sync.Mutex
	healthCache map[int64]cachedGroupHealth

	startOnce sync.Once
	stopCh    chan struct{}
	stopOnce  sync.Once
	wg        sync.WaitGroup
}

// NewMetricsAggregator 创建小时聚合器。调用 Start 后按整点调度。
func NewMetricsAggregator(repo GroupMetricRepository) *MetricsAggregator {
	return &MetricsAggregator{
		repo:        repo,
		now:         time.Now,
		healthCache: make(map[int64]cachedGroupHealth),
		stopCh:      make(chan struct{}),
	}
}

// AggregateHour 重算并写入 hour 所在的整点小时。
//
// 参数可以是任意时刻，内部统一截断到 UTC 整点；同一小时重复调用会覆盖旧值，
// 因此迟到/补录的 request_metrics 可以通过重跑修正聚合结果。
func (a *MetricsAggregator) AggregateHour(ctx context.Context, hour time.Time) error {
	if a == nil || a.repo == nil {
		return ErrMetricsAggregatorUnavailable
	}
	if hour.IsZero() {
		return ErrMetricsHourRequired
	}
	hour = hour.UTC().Truncate(time.Hour)

	metrics, err := a.repo.AggregateRequestMetrics(ctx, hour, hour.Add(time.Hour))
	if err != nil {
		return err
	}
	return a.repo.UpsertGroupMetrics(ctx, hour, metrics)
}

// Health 读取最近 1 小时与最近 15 分钟，返回是否降级。
//
// 规则（顺序即优先级）：
//  1. 最近 15 分钟有请求但没有任何成功请求 → 中断，不论样本量；
//  2. 最近 1 小时请求数 >= 20 且上游失败率 > 10%；
//  3. 最近 1 小时请求数 >= 20 且流式首包 P95 > 10s。
func (a *MetricsAggregator) Health(ctx context.Context, groupID int64) (Health, error) {
	healths, err := a.HealthMany(ctx, []int64{groupID})
	if err != nil {
		return Health{}, err
	}
	return healths[groupID], nil
}

// HealthMany 批量判定分组降级状态，货架用它避免逐分组查询的 N+1。
//
// 结果按分组共享缓存 MarketHealthCacheTTL（30 秒）：同一批货架请求、以及 30 秒内
// 的后续请求都不会重复扫描 request_metrics。缓存只保留成功结果，查询失败不缓存，
// 下一次请求会立即重试。
func (a *MetricsAggregator) HealthMany(ctx context.Context, groupIDs []int64) (map[int64]Health, error) {
	out := make(map[int64]Health, len(groupIDs))
	if a == nil || a.repo == nil {
		return out, ErrMetricsAggregatorUnavailable
	}
	if len(groupIDs) == 0 {
		return out, nil
	}

	now := a.now().UTC()
	missing := make([]int64, 0, len(groupIDs))
	seen := make(map[int64]struct{}, len(groupIDs))

	a.healthMu.Lock()
	if a.healthCache == nil {
		a.healthCache = make(map[int64]cachedGroupHealth)
	}
	for groupID, entry := range a.healthCache {
		if !now.Before(entry.expiresAt) {
			delete(a.healthCache, groupID)
		}
	}
	for _, groupID := range groupIDs {
		if _, duplicate := seen[groupID]; duplicate {
			continue
		}
		seen[groupID] = struct{}{}
		if entry, ok := a.healthCache[groupID]; ok && now.Before(entry.expiresAt) {
			out[groupID] = entry.health
			continue
		}
		missing = append(missing, groupID)
	}
	a.healthMu.Unlock()

	if len(missing) > 0 {
		samples, err := a.repo.GroupHealthSamples(
			ctx,
			missing,
			now.Add(-time.Hour),
			now.Add(-DegradedNoSuccessWindow),
		)
		if err != nil {
			return nil, err
		}

		expiresAt := now.Add(MarketHealthCacheTTL)
		a.healthMu.Lock()
		for _, groupID := range missing {
			health := evaluateGroupHealth(samples[groupID])
			out[groupID] = health
			a.healthCache[groupID] = cachedGroupHealth{health: health, expiresAt: expiresAt}
		}
		a.healthMu.Unlock()
	}
	return out, nil
}

func evaluateGroupHealth(sample GroupHealthSample) Health {
	health := Health{SampleSize: sample.Requests}
	if sample.RecentRequests > 0 && sample.RecentSuccesses == 0 {
		health.Degraded = true
		health.Reason = DegradedReasonNoRecentSuccess
		return health
	}
	if sample.Requests >= DegradedMinSamples {
		failureRate := float64(sample.UpstreamFailures) / float64(sample.Requests)
		if failureRate > DegradedMaxUpstreamFailureRate {
			health.Degraded = true
			health.Reason = DegradedReasonUpstreamFailureRate
			return health
		}
		if sample.P95TTFT > DegradedMaxP95TTFTMs {
			health.Degraded = true
			health.Reason = DegradedReasonSlowTTFT
		}
	}
	return health
}

// GroupMetrics 返回指定分组最近 1 小时与最近 24 小时的聚合摘要。
//
// 窗口锚点固定为 now.Truncate(time.Hour)，而不是“该分组的最新桶”：
//   - last_hour 只取 [currentHour-1h, currentHour)；
//   - last_24h 只汇总 [currentHour-24h, currentHour)。
//
// 停更超过 1 小时的分组不会有 last_hour；停更超过 24 小时的分组整条摘要都不存在。
// 窗口内没有行时对应字段为 nil（对外 JSON 为 null），绝不把旧桶冒充当前窗口。
func (a *MetricsAggregator) GroupMetrics(ctx context.Context, groupIDs []int64) (map[int64]GroupMetricSummary, error) {
	out := make(map[int64]GroupMetricSummary, len(groupIDs))
	if a == nil || a.repo == nil || len(groupIDs) == 0 {
		return out, nil
	}

	now := a.now().UTC()
	currentHour := now.Truncate(time.Hour)
	windowStart := currentHour.Add(-24 * time.Hour)
	rows, err := a.repo.ListGroupMetrics(ctx, groupIDs, windowStart, currentHour)
	if err != nil {
		return nil, err
	}

	lastHourStart := currentHour.Add(-time.Hour)
	byGroup := make(map[int64][]GroupMetric)
	for _, row := range rows {
		row.Hour = row.Hour.UTC().Truncate(time.Hour)
		// 只接受已经完整结束且位于 24h 窗口内的小时桶。
		if row.Hour.Before(windowStart) || !row.Hour.Before(currentHour) {
			continue
		}
		byGroup[row.GroupID] = append(byGroup[row.GroupID], row)
	}

	for groupID, groupRows := range byGroup {
		latest := groupRows[0].Hour
		hourRows := make([]GroupMetric, 0, 1)
		for _, row := range groupRows {
			if row.Hour.After(latest) {
				latest = row.Hour
			}
			if row.Hour.Equal(lastHourStart) {
				hourRows = append(hourRows, row)
			}
		}

		asOf := latest.Add(time.Hour)
		dayWindow := summarizeGroupMetricWindow(groupRows, len(groupRows) == 1)

		var hourWindow *GroupMetricWindow
		if len(hourRows) > 0 {
			window := summarizeGroupMetricWindow(hourRows, len(hourRows) == 1)
			hourWindow = &window
		}

		out[groupID] = GroupMetricSummary{
			LastHour:   hourWindow,
			Last24h:    &dayWindow,
			AsOf:       &asOf,
			SampleSize: dayWindow.Requests,
		}
	}
	return out, nil
}

// summarizeGroupMetricWindow 汇总小时行。计数、token 与比率可以精确相加；
// 分位数只有在单桶时才可精确给出。
func summarizeGroupMetricWindow(rows []GroupMetric, includePercentiles bool) GroupMetricWindow {
	var window GroupMetricWindow
	var inputTokens, cacheReadTokens, cacheCreationTokens int64
	for _, row := range rows {
		window.Requests += row.Requests
		window.UpstreamFailures += row.UpstreamFailures
		window.RequestErrors += row.RequestErrors
		window.TTFTSamples += row.TTFTSamples
		inputTokens += row.InputTokens
		cacheReadTokens += row.CacheReadTokens
		cacheCreationTokens += row.CacheCreationTokens
	}
	if window.Requests > 0 {
		window.SuccessRate = float64(window.Requests-window.UpstreamFailures) / float64(window.Requests)
		window.RequestErrorRate = float64(window.RequestErrors) / float64(window.Requests)
	}
	denominator := inputTokens + cacheReadTokens + cacheCreationTokens
	if denominator > 0 {
		window.CacheHitRate = float64(cacheReadTokens) / float64(denominator)
		window.HasCache = true
	}
	if includePercentiles && len(rows) == 1 && rows[0].TTFTSamples > 0 {
		window.P50TTFT = rows[0].P50TTFT
		window.P95TTFT = rows[0].P95TTFT
		window.HasTTFT = true
	}
	return window
}

// Start 启动小时聚合任务：启动时从最后已聚合小时补到上一个完整小时，
// 此后每小时第 5 分钟继续补算。跨小时停机或单次失败留下的缺口会被重试。
func (a *MetricsAggregator) Start() {
	if a == nil || a.repo == nil {
		return
	}
	a.startOnce.Do(func() {
		a.wg.Add(1)
		go func() {
			defer a.wg.Done()

			a.runOnce()

			timer := time.NewTimer(durationUntilNextRun(a.now()))
			defer timer.Stop()
			for {
				select {
				case <-timer.C:
					a.runOnce()
					// 若定时器在 :05 边界前几毫秒提前触发，重排后会落在同一个 :05；
					// 重算是幂等的，不会重复计数。
					timer.Reset(durationUntilNextRun(a.now()))
				case <-a.stopCh:
					return
				}
			}
		}()
	})
}

// Stop 停止聚合任务；重复调用安全。
func (a *MetricsAggregator) Stop() {
	if a == nil {
		return
	}
	a.stopOnce.Do(func() { close(a.stopCh) })
	a.wg.Wait()
}

// runOnce 从下一个待聚合小时补到上一个完整小时。
//
// nextHour 在第一次运行时由小时表的 MAX(hour) 恢复（空表则从上一个完整小时开始），
// 之后只在单小时聚合成功后才前进。任何一次失败都会停在失败的小时，下一次 tick
// 从同一小时重试；回看范围有界为 MetricsAggregationLookback（48h）。
func (a *MetricsAggregator) runOnce() {
	if a == nil || a.repo == nil {
		return
	}

	a.aggregateMu.Lock()
	defer a.aggregateMu.Unlock()

	now := a.now().UTC()
	currentHour := now.Truncate(time.Hour)
	lastCompleteHour := currentHour.Add(-time.Hour)
	earliestHour := currentHour.Add(-MetricsAggregationLookback)

	if a.nextHour.IsZero() {
		queryCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		latest, err := a.repo.LatestGroupMetricHour(queryCtx)
		cancel()
		if err != nil {
			log.Printf("[MetricsAggregator] load latest aggregated hour failed: %v", err)
			return
		}
		if latest == nil {
			// 全新小时表：只聚合上一个完整小时，不做无意义的历史扫描。
			a.nextHour = lastCompleteHour
		} else {
			a.nextHour = latest.UTC().Truncate(time.Hour).Add(time.Hour)
		}
	}

	if a.nextHour.Before(earliestHour) {
		a.nextHour = earliestHour
	}
	if a.nextHour.After(lastCompleteHour) {
		return
	}

	for hour := a.nextHour; !hour.After(lastCompleteHour); hour = hour.Add(time.Hour) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		err := a.AggregateHour(ctx, hour)
		cancel()
		if err != nil {
			log.Printf("[MetricsAggregator] aggregate hour %s failed: %v", hour.Format(time.RFC3339), err)
			return
		}
		a.nextHour = hour.Add(time.Hour)
	}
}

// nextAggregationRun 返回 now 之后最近的一个 HH:05:00（UTC）。
func nextAggregationRun(now time.Time) time.Time {
	now = now.UTC()
	next := time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), 5, 0, 0, time.UTC)
	if !next.After(now) {
		next = next.Add(time.Hour)
	}
	return next
}

func durationUntilNextRun(now time.Time) time.Duration {
	delay := nextAggregationRun(now).Sub(now.UTC())
	if delay <= 0 {
		return time.Hour
	}
	return delay
}
