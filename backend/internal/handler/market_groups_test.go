package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/woolsen/router2api/internal/server/middleware"
	"github.com/woolsen/router2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// fakeMarketGroupSource 是货架 handler 的数据源替身：直接返回构造好的广场分组。
type fakeMarketGroupSource struct {
	groups []service.PlazaGroup
	err    error
}

func (f *fakeMarketGroupSource) ListGroups(context.Context) ([]service.PlazaGroup, error) {
	return f.groups, f.err
}

// fakeMarketVisibility 是可见性替身：模拟 APIKeyService.GetUserGroupVisibility
// 与 GetUserGroupRates。
type fakeMarketVisibility struct {
	allowed        map[int64]struct{}
	restrictPublic bool
	err            error
	rates          map[int64]float64
	ratesErr       error
}

func (f *fakeMarketVisibility) GetUserGroupVisibility(context.Context, int64) (map[int64]struct{}, bool, error) {
	return f.allowed, f.restrictPublic, f.err
}

func (f *fakeMarketVisibility) GetUserGroupRates(context.Context, int64) (map[int64]float64, error) {
	return f.rates, f.ratesErr
}

// newFakeMarketVisibility 返回一个“登录但没有任何专属授权、未开启公开分组限制”
// 的可见性替身：非专属分组可见，专属分组不可见。
func newFakeMarketVisibility() *fakeMarketVisibility {
	return &fakeMarketVisibility{allowed: map[int64]struct{}{}}
}

// marketGroupResponse 是测试侧的解码形状。Stats 用 any 而不是指针：
// JSON 里的 null 解码后为 nil，而字段缺失同样为 nil，所以另用原始 JSON 断言键存在。
type marketGroupResponse struct {
	ID             int64                `json:"id"`
	Name           string               `json:"name"`
	Platform       string               `json:"platform"`
	RateMultiplier float64              `json:"rate_multiplier"`
	RateIsEstimate bool                 `json:"rate_is_estimate"`
	PeakRate       *marketPeakRateDTO   `json:"peak_rate"`
	Models         []string             `json:"models"`
	Status         string               `json:"status"`
	Stats          *marketGroupStatsDTO `json:"stats"`
	SampleSize     int                  `json:"sample_size"`
	Degraded       bool                 `json:"degraded"`
	DegradedReason string               `json:"degraded_reason"`
	AsOf           *time.Time           `json:"as_of"`
}

// marketGroupStatsDTO 对应 1h + 24h 两档指标。
type marketGroupStatsDTO struct {
	LastHour *marketStatsWindowDTO `json:"last_hour"`
	Last24h  *marketStatsWindowDTO `json:"last_24h"`
}

// marketStatsWindowDTO 是一档窗口；分位数与缓存率允许为 null。
type marketStatsWindowDTO struct {
	Requests         int      `json:"requests"`
	UpstreamFailures int      `json:"upstream_failures"`
	SuccessRate      float64  `json:"success_rate"`
	RequestErrorRate float64  `json:"request_error_rate"`
	TTFTSamples      int      `json:"ttft_samples"`
	P50TTFTMs        *float64 `json:"p50_ttft_ms"`
	P95TTFTMs        *float64 `json:"p95_ttft_ms"`
	CacheHitRate     *float64 `json:"cache_hit_rate"`
}

// fakeMarketMetrics 是货架指标来源替身。
type fakeMarketMetrics struct {
	summaries map[int64]service.GroupMetricSummary
	health    map[int64]service.Health
	err       error
	healthErr error
}

func (f *fakeMarketMetrics) GroupMetrics(context.Context, []int64) (map[int64]service.GroupMetricSummary, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.summaries, nil
}

func (f *fakeMarketMetrics) HealthMany(_ context.Context, groupIDs []int64) (map[int64]service.Health, error) {
	if f.healthErr != nil {
		return nil, f.healthErr
	}
	out := make(map[int64]service.Health, len(groupIDs))
	for _, groupID := range groupIDs {
		out[groupID] = f.health[groupID]
	}
	return out, nil
}

// marketPeakRateDTO 与 handler 的 marketPeakRate 同形，测试侧独立声明避免耦合实现。
type marketPeakRateDTO struct {
	Start      string  `json:"start"`
	End        string  `json:"end"`
	Multiplier float64 `json:"multiplier"`
	ActiveNow  bool    `json:"active_now"`
}

// doGET 模拟一次带上登录身份的 GET /api/v1/market/groups。
func doGET(t *testing.T, source MarketGroupSource) ([]marketGroupResponse, []byte) {
	t.Helper()
	return doGETWithVisibility(t, source, newFakeMarketVisibility())
}

func doGETWithVisibility(t *testing.T, source MarketGroupSource, visibility MarketVisibilitySource) ([]marketGroupResponse, []byte) {
	t.Helper()
	return doGETWithMetrics(t, source, visibility, nil)
}

func doGETWithMetrics(
	t *testing.T,
	source MarketGroupSource,
	visibility MarketVisibilitySource,
	metrics MarketMetricsSource,
) ([]marketGroupResponse, []byte) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h := NewMarketGroupsHandler(source, visibility)
	h.SetMetricsSource(metrics)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/market/groups", nil)
	// middleware.ContextKeyUser 是 "user"；这里直接写入认证主体，等价于 JWT 中间件。
	c.Set("user", middleware.AuthSubject{UserID: 7})

	h.List(c)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var envelope struct {
		Data []marketGroupResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope), "body: %s", w.Body.String())
	return envelope.Data, w.Body.Bytes()
}

// marketFixture 覆盖四种分组：两个普通入驻商户分组（倍率乱序）、一个专属入驻商户分组、
// 一个平台自营分组。平台自营分组与未授权的专属分组都必须被货架过滤掉。
func marketFixture() []service.PlazaGroup {
	merchantA := int64(101)
	merchantB := int64(202)
	return []service.PlazaGroup{
		{
			ID:                 2,
			Name:               "B 商户分组",
			Platform:           service.PlatformAnthropic,
			SubscriptionType:   service.SubscriptionTypeSubscription,
			RateMultiplier:     0.3,
			PeakRateEnabled:    true,
			PeakStart:          "00:00",
			PeakEnd:            "23:59",
			PeakRateMultiplier: 1.5,
			MerchantID:         &merchantB,
			Models: []service.PlazaModel{
				{Name: "claude-opus-4-1", Platform: service.PlatformAnthropic},
			},
		},
		{
			ID:             1,
			Name:           "A 商户分组",
			Platform:       service.PlatformAnthropic,
			RateMultiplier: 0.2,
			MerchantID:     &merchantA,
			Models: []service.PlazaModel{
				{Name: "claude-sonnet-4-5", Platform: service.PlatformAnthropic},
				{Name: "claude-haiku-4-5", Platform: service.PlatformAnthropic},
			},
		},
		{
			ID:             3,
			Name:           "平台自营分组",
			Platform:       service.PlatformAnthropic,
			RateMultiplier: 0.1,
			MerchantID:     nil,
			Models: []service.PlazaModel{
				{Name: "claude-opus-4-1", Platform: service.PlatformAnthropic},
			},
		},
		{
			ID:             4,
			Name:           "专属商户分组",
			Platform:       service.PlatformAnthropic,
			RateMultiplier: 0.25,
			IsExclusive:    true,
			MerchantID:     &merchantA,
			Models: []service.PlazaModel{
				{Name: "claude-opus-4-1", Platform: service.PlatformAnthropic},
			},
		},
	}
}

func TestMarketGroupsHidesStatsBelowFiftySamples(t *testing.T) {
	asOf := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	metrics := &fakeMarketMetrics{
		summaries: map[int64]service.GroupMetricSummary{
			1: {
				LastHour:   &service.GroupMetricWindow{Requests: 10, SuccessRate: 1},
				Last24h:    &service.GroupMetricWindow{Requests: 49, SuccessRate: 1},
				AsOf:       &asOf,
				SampleSize: 49,
			},
		},
		health: map[int64]service.Health{},
	}
	resp, raw := doGETWithMetrics(t, &fakeMarketGroupSource{groups: marketFixture()}, newFakeMarketVisibility(), metrics)
	require.NotEmpty(t, resp)

	var group *marketGroupResponse
	for i := range resp {
		if resp[i].ID == 1 {
			group = &resp[i]
			break
		}
	}
	require.NotNil(t, group)
	require.Nil(t, group.Stats, "49 samples must render as insufficient data")
	require.Equal(t, 49, group.SampleSize)
	require.NotNil(t, group.AsOf, "as_of stays honest even when stats are withheld")

	var envelope struct {
		Data []map[string]json.RawMessage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(raw, &envelope))
	for i := range envelope.Data {
		if string(envelope.Data[i]["id"]) == "1" {
			require.JSONEq(t, "null", string(envelope.Data[i]["stats"]))
		}
	}
}

func TestMarketGroupsExposesStatsAtFiftySamples(t *testing.T) {
	asOf := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	cacheHit := 0.75
	p50 := 420.0
	p95 := 1200.0
	metrics := &fakeMarketMetrics{
		summaries: map[int64]service.GroupMetricSummary{
			1: {
				LastHour: &service.GroupMetricWindow{
					Requests:         12,
					UpstreamFailures: 1,
					RequestErrors:    2,
					TTFTSamples:      10,
					SuccessRate:      11.0 / 12.0,
					RequestErrorRate: 2.0 / 12.0,
					P50TTFT:          p50,
					P95TTFT:          p95,
					HasTTFT:          true,
					CacheHitRate:     cacheHit,
					HasCache:         true,
				},
				Last24h: &service.GroupMetricWindow{
					Requests:         50,
					UpstreamFailures: 3,
					RequestErrors:    4,
					TTFTSamples:      50,
					SuccessRate:      47.0 / 50.0,
					RequestErrorRate: 4.0 / 50.0,
					CacheHitRate:     0.6,
					HasCache:         true,
				},
				AsOf:       &asOf,
				SampleSize: 50,
			},
		},
		health: map[int64]service.Health{},
	}
	resp, _ := doGETWithMetrics(t, &fakeMarketGroupSource{groups: marketFixture()}, newFakeMarketVisibility(), metrics)
	var group *marketGroupResponse
	for i := range resp {
		if resp[i].ID == 1 {
			group = &resp[i]
			break
		}
	}
	require.NotNil(t, group)
	require.NotNil(t, group.Stats)
	require.Equal(t, 50, group.SampleSize)
	require.NotNil(t, group.AsOf)
	require.True(t, group.AsOf.Equal(asOf))

	require.NotNil(t, group.Stats.LastHour)
	require.Equal(t, 12, group.Stats.LastHour.Requests)
	require.Equal(t, 1, group.Stats.LastHour.UpstreamFailures)
	require.InDelta(t, 11.0/12.0, group.Stats.LastHour.SuccessRate, 1e-9)
	require.InDelta(t, 2.0/12.0, group.Stats.LastHour.RequestErrorRate, 1e-9)
	require.NotNil(t, group.Stats.LastHour.P50TTFTMs)
	require.InDelta(t, p50, *group.Stats.LastHour.P50TTFTMs, 1e-9)
	require.NotNil(t, group.Stats.LastHour.P95TTFTMs)
	require.InDelta(t, p95, *group.Stats.LastHour.P95TTFTMs, 1e-9)
	require.NotNil(t, group.Stats.LastHour.CacheHitRate)
	require.InDelta(t, cacheHit, *group.Stats.LastHour.CacheHitRate, 1e-9)

	require.NotNil(t, group.Stats.Last24h)
	require.Equal(t, 50, group.Stats.Last24h.Requests)
	require.Nil(t, group.Stats.Last24h.P95TTFTMs, "multi-bucket percentiles must not be fabricated")
}

func TestMarketGroupsMarksDegradedWithoutDelisting(t *testing.T) {
	asOf := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	metrics := &fakeMarketMetrics{
		summaries: map[int64]service.GroupMetricSummary{
			1: {
				LastHour:   &service.GroupMetricWindow{Requests: 20, UpstreamFailures: 3, SuccessRate: 0.85},
				Last24h:    &service.GroupMetricWindow{Requests: 55, UpstreamFailures: 5, SuccessRate: 50.0 / 55.0},
				AsOf:       &asOf,
				SampleSize: 55,
			},
		},
		health: map[int64]service.Health{
			1: {Degraded: true, Reason: service.DegradedReasonUpstreamFailureRate, SampleSize: 20},
		},
	}
	resp, _ := doGETWithMetrics(t, &fakeMarketGroupSource{groups: marketFixture()}, newFakeMarketVisibility(), metrics)

	var group *marketGroupResponse
	for i := range resp {
		if resp[i].ID == 1 {
			group = &resp[i]
			break
		}
	}
	require.NotNil(t, group, "a degraded group must stay on the shelf")
	require.True(t, group.Degraded)
	require.Equal(t, service.DegradedReasonUpstreamFailureRate, group.DegradedReason)
	require.Equal(t, service.StatusActive, group.Status, "degraded must not change status")
	require.NotNil(t, group.Stats, "degraded does not hide stats")
}

func TestMarketGroupsMetricsSourceError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewMarketGroupsHandler(&fakeMarketGroupSource{groups: marketFixture()}, newFakeMarketVisibility())
	h.SetMetricsSource(&fakeMarketMetrics{err: errors.New("metrics down")})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/market/groups", nil)
	c.Set("user", middleware.AuthSubject{UserID: 7})

	h.List(c)

	require.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestMarketGroupsHealthSourceError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewMarketGroupsHandler(&fakeMarketGroupSource{groups: marketFixture()}, newFakeMarketVisibility())
	h.SetMetricsSource(&fakeMarketMetrics{
		summaries: map[int64]service.GroupMetricSummary{},
		healthErr: errors.New("health down"),
	})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/market/groups", nil)
	c.Set("user", middleware.AuthSubject{UserID: 7})

	h.List(c)

	require.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestMarketGroupsOmitsStatsInM1(t *testing.T) {
	resp, raw := doGET(t, &fakeMarketGroupSource{groups: marketFixture()})
	require.NotEmpty(t, resp)
	for i := range resp {
		require.Nil(t, resp[i].Stats, "M1 must not expose stats before request_metrics exists")
	}

	// 字段必须存在且为 null，客户端据此显示「数据不足」；不能整键省略。
	var envelope struct {
		Data []map[string]json.RawMessage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(raw, &envelope))
	stats, ok := envelope.Data[0]["stats"]
	require.True(t, ok, "stats key must be present")
	require.JSONEq(t, "null", string(stats))
}

func TestMarketGroupsExposesRatioAndModels(t *testing.T) {
	resp, _ := doGET(t, &fakeMarketGroupSource{groups: marketFixture()})
	require.NotEmpty(t, resp)

	// 倍率升序后第一条是 0.2 的 A 分组。
	require.Equal(t, 0.2, resp[0].RateMultiplier)
	require.Equal(t, int64(1), resp[0].ID)
	require.Equal(t, service.StatusActive, resp[0].Status)
	require.ElementsMatch(t, []string{"claude-sonnet-4-5", "claude-haiku-4-5"}, resp[0].Models)
	require.NotEmpty(t, resp[0].Models, "models must not be empty")
}

func TestMarketGroupsFiltersPlatformOwnedGroups(t *testing.T) {
	resp, _ := doGET(t, &fakeMarketGroupSource{groups: marketFixture()})
	require.Len(t, resp, 2, "only merchant-owned groups belong on the shelf")
	for i := range resp {
		require.NotEqual(t, int64(3), resp[i].ID, "platform-owned group must not be exposed")
	}
}

func TestMarketGroupsSortsByRateAscending(t *testing.T) {
	resp, _ := doGET(t, &fakeMarketGroupSource{groups: marketFixture()})
	require.Len(t, resp, 2)
	require.LessOrEqual(t, resp[0].RateMultiplier, resp[1].RateMultiplier)
	require.Equal(t, int64(1), resp[0].ID)
	require.Equal(t, int64(2), resp[1].ID)
}

func TestMarketGroupsHidesUngrantedExclusiveGroup(t *testing.T) {
	// 普通登录用户没有任何专属授权：专属商户分组的名称与倍率都不能泄漏到货架。
	resp, _ := doGET(t, &fakeMarketGroupSource{groups: marketFixture()})
	for i := range resp {
		require.NotEqual(t, int64(4), resp[i].ID, "exclusive group must not leak to a user without a grant")
	}
	require.Len(t, resp, 2)
}

func TestMarketGroupsShowsGrantedExclusiveGroup(t *testing.T) {
	resp, _ := doGETWithVisibility(t, &fakeMarketGroupSource{groups: marketFixture()}, &fakeMarketVisibility{
		allowed: map[int64]struct{}{4: {}},
	})
	ids := make([]int64, 0, len(resp))
	for i := range resp {
		ids = append(ids, resp[i].ID)
	}
	require.ElementsMatch(t, []int64{1, 2, 4}, ids, "granted exclusive group must be visible")
}

func TestMarketGroupsRespectsRestrictPublicGroups(t *testing.T) {
	// 用户开启公开分组限制后，未授权的公开分组同样不上架，只剩授权集合内的分组。
	resp, _ := doGETWithVisibility(t, &fakeMarketGroupSource{groups: marketFixture()}, &fakeMarketVisibility{
		allowed:        map[int64]struct{}{1: {}},
		restrictPublic: true,
	})
	require.Len(t, resp, 1)
	require.Equal(t, int64(1), resp[0].ID)
}

func TestMarketGroupsHidesMerchantIdentityAndUpstream(t *testing.T) {
	_, raw := doGET(t, &fakeMarketGroupSource{groups: marketFixture()})

	var envelope struct {
		Data []map[string]json.RawMessage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(raw, &envelope))
	require.NotEmpty(t, envelope.Data)

	forbidden := []string{
		"merchant_id", "merchant", "merchant_name",
		"upstream", "upstream_url", "base_url", "api_base_url", "channel", "channels",
	}
	for _, key := range forbidden {
		_, exists := envelope.Data[0][key]
		require.Falsef(t, exists, "market DTO must not expose %q", key)
	}

	allowed := []string{
		"id", "name", "platform", "rate_multiplier", "rate_is_estimate",
		"peak_rate", "models", "status", "stats", "sample_size",
		"degraded", "degraded_reason", "as_of",
	}
	require.Len(t, envelope.Data[0], len(allowed), "unexpected extra fields in market DTO")
	for _, key := range allowed {
		_, exists := envelope.Data[0][key]
		require.Truef(t, exists, "market DTO must expose %q", key)
	}
}

func TestMarketGroupsUsesUserRateAndSortsByIt(t *testing.T) {
	// 用户专属倍率覆盖基础倍率（与网关一致）：B 分组基础 0.3，用户专属 0.05，
	// 应当排在 A 分组（0.2）前面，且展示 0.05 而不是 0.3。
	resp, _ := doGETWithVisibility(t, &fakeMarketGroupSource{groups: marketFixture()}, &fakeMarketVisibility{
		allowed: map[int64]struct{}{},
		rates:   map[int64]float64{2: 0.05},
	})
	require.Len(t, resp, 2)
	require.Equal(t, int64(2), resp[0].ID)
	require.InDelta(t, 0.05, resp[0].RateMultiplier, 1e-12)
	require.False(t, resp[0].RateIsEstimate)
	require.Equal(t, int64(1), resp[1].ID)
	require.InDelta(t, 0.2, resp[1].RateMultiplier, 1e-12)
}

func TestMarketGroupsMarksRateEstimateWhenUserRatesUnavailable(t *testing.T) {
	resp, _ := doGETWithVisibility(t, &fakeMarketGroupSource{groups: marketFixture()}, &fakeMarketVisibility{
		allowed:  map[int64]struct{}{},
		ratesErr: errors.New("rate store down"),
	})
	require.NotEmpty(t, resp)
	for i := range resp {
		require.True(t, resp[i].RateIsEstimate,
			"falling back to the group rate must be flagged as an estimate")
	}
}

func TestMarketGroupsExposesPeakRateSeparately(t *testing.T) {
	resp, _ := doGET(t, &fakeMarketGroupSource{groups: marketFixture()})
	var groupB *marketGroupResponse
	for i := range resp {
		if resp[i].ID == 2 {
			groupB = &resp[i]
			break
		}
	}
	require.NotNil(t, groupB)
	require.NotNil(t, groupB.PeakRate)
	require.Equal(t, "00:00", groupB.PeakRate.Start)
	require.Equal(t, "23:59", groupB.PeakRate.End)
	require.InDelta(t, 1.5, groupB.PeakRate.Multiplier, 1e-12)
	// 倍率本身不并入峰时因子；峰时单独标注，由前端提示。
	require.InDelta(t, 0.3, groupB.RateMultiplier, 1e-12)
}

func TestMarketGroupsEmptyShelfIsArray(t *testing.T) {
	_, raw := doGET(t, &fakeMarketGroupSource{})
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(raw, &envelope))
	require.JSONEq(t, "[]", string(envelope.Data), "empty shelf must serialize as [] not null")
}

func TestMarketGroupsSourceError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewMarketGroupsHandler(&fakeMarketGroupSource{err: errors.New("boom")}, newFakeMarketVisibility())
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/market/groups", nil)
	c.Set("user", middleware.AuthSubject{UserID: 7})

	h.List(c)

	require.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestMarketGroupsVisibilityError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewMarketGroupsHandler(&fakeMarketGroupSource{groups: marketFixture()}, &fakeMarketVisibility{err: errors.New("boom")})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/market/groups", nil)
	c.Set("user", middleware.AuthSubject{UserID: 7})

	h.List(c)

	require.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestMarketGroupsRequiresAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewMarketGroupsHandler(&fakeMarketGroupSource{groups: marketFixture()}, newFakeMarketVisibility())
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/market/groups", nil)

	h.List(c)

	require.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestMarketGroupsMarksEstimateWhenImageOrVideoRateIsIndependent(t *testing.T) {
	merchantID := int64(101)
	groups := []service.PlazaGroup{{
		ID:                   9,
		Name:                 "image-independent",
		Platform:             service.PlatformAnthropic,
		RateMultiplier:       0.2,
		ImageRateIndependent: true,
		MerchantID:           &merchantID,
		Models:               []service.PlazaModel{{Name: "claude-sonnet-4-5", Platform: service.PlatformAnthropic}},
	}}
	resp, _ := doGET(t, &fakeMarketGroupSource{groups: groups})
	require.Len(t, resp, 1)
	require.True(t, resp[0].RateIsEstimate,
		"a group with an independent image rate cannot be represented by one token rate")
}
