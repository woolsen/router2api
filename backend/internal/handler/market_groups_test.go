package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

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

// marketGroupResponse 是测试侧的解码形状。Stats 用 any 而不是指针：
// JSON 里的 null 解码后为 nil，而字段缺失同样为 nil，所以另用原始 JSON 断言键存在。
type marketGroupResponse struct {
	ID             int64    `json:"id"`
	Name           string   `json:"name"`
	Platform       string   `json:"platform"`
	RateMultiplier float64  `json:"rate_multiplier"`
	Models         []string `json:"models"`
	Status         string   `json:"status"`
	Stats          any      `json:"stats"`
}

// doGET 模拟一次 GET /api/v1/market/groups，返回解码后的 data 与原始响应体。
func doGET(t *testing.T, source MarketGroupSource) ([]marketGroupResponse, []byte) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h := NewMarketGroupsHandler(source)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/market/groups", nil)

	h.List(c)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var envelope struct {
		Data []marketGroupResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope), "body: %s", w.Body.String())
	return envelope.Data, w.Body.Bytes()
}

// marketFixture 覆盖三种分组：两个入驻商户分组（倍率乱序）与一个平台自营分组。
// 平台自营分组必须被货架过滤掉。
func marketFixture() []service.PlazaGroup {
	merchantA := int64(101)
	merchantB := int64(202)
	return []service.PlazaGroup{
		{
			ID:             2,
			Name:           "B 商户分组",
			Platform:       service.PlatformAnthropic,
			RateMultiplier: 0.3,
			MerchantID:     &merchantB,
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
	}
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

	allowed := []string{"id", "name", "platform", "rate_multiplier", "models", "status", "stats"}
	require.Len(t, envelope.Data[0], len(allowed), "unexpected extra fields in market DTO")
	for _, key := range allowed {
		_, exists := envelope.Data[0][key]
		require.Truef(t, exists, "market DTO must expose %q", key)
	}
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
	h := NewMarketGroupsHandler(&fakeMarketGroupSource{err: errors.New("boom")})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/market/groups", nil)

	h.List(c)

	require.Equal(t, http.StatusInternalServerError, w.Code)
}
