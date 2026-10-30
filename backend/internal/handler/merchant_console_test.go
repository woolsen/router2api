package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/woolsen/router2api/internal/server/middleware"
	"github.com/woolsen/router2api/internal/service"
)

type fakeMerchantConsoleService struct {
	dashboard *service.MerchantDashboard
	trend     *service.MerchantUsageTrend
	err       error

	gotMerchantID int64
	gotDays       int
}

func (f *fakeMerchantConsoleService) Dashboard(_ context.Context, merchantID int64) (*service.MerchantDashboard, error) {
	f.gotMerchantID = merchantID
	return f.dashboard, f.err
}

func (f *fakeMerchantConsoleService) DailyUsage(_ context.Context, merchantID int64, days int) (*service.MerchantUsageTrend, error) {
	f.gotMerchantID = merchantID
	f.gotDays = days
	return f.trend, f.err
}

func newMerchantConsoleTestRouter(t *testing.T, svc service.MerchantConsoleReader, merchantID *int64) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)

	h := NewMerchantConsoleHandler(svc)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		if merchantID != nil {
			c.Set(string(middleware.ContextKeyMerchantID), *merchantID)
		}
		c.Next()
	})
	r.GET("/api/v1/merchant/me", h.Me)
	r.GET("/api/v1/merchant/usage/daily", h.DailyUsage)
	return r
}

func TestMerchantConsoleMeReturnsAuthenticatedMerchant(t *testing.T) {
	svc := &fakeMerchantConsoleService{
		dashboard: &service.MerchantDashboard{
			MerchantID:     5,
			Name:           "上游一号",
			CommissionRate: 0.08,
			PayableBalance: 12.34,
		},
	}
	id := int64(5)
	r := newMerchantConsoleTestRouter(t, svc, &id)

	w := httptest.NewRecorder()
	// 客户端传入的 merchant_id 必须被忽略，处理器只认中间件注入值。
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/merchant/me?merchant_id=999", nil))

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, int64(5), svc.gotMerchantID)

	var body struct {
		Code int `json:"code"`
		Data struct {
			MerchantID     int64   `json:"merchant_id"`
			Name           string  `json:"name"`
			CommissionRate float64 `json:"commission_rate"`
			PayableBalance float64 `json:"payable_balance"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Equal(t, 0, body.Code)
	require.Equal(t, int64(5), body.Data.MerchantID)
	require.Equal(t, "上游一号", body.Data.Name)
	require.InDelta(t, 0.08, body.Data.CommissionRate, 1e-9)
	require.InDelta(t, 12.34, body.Data.PayableBalance, 1e-9)
}

func TestMerchantConsoleMeRejectsMissingScope(t *testing.T) {
	r := newMerchantConsoleTestRouter(t, &fakeMerchantConsoleService{}, nil)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/merchant/me", nil))

	require.Equal(t, http.StatusForbidden, w.Code)
	require.Contains(t, w.Body.String(), `"reason":"MERCHANT_ONLY"`)
}

func TestMerchantConsoleMeMapsMerchantNotFound(t *testing.T) {
	id := int64(5)
	r := newMerchantConsoleTestRouter(t, &fakeMerchantConsoleService{err: service.ErrMerchantNotFound}, &id)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/merchant/me", nil))

	require.Equal(t, http.StatusNotFound, w.Code)
	require.Contains(t, w.Body.String(), `"reason":"MERCHANT_NOT_FOUND"`)
}

func TestMerchantConsoleDailyUsageDefaultsToSevenDays(t *testing.T) {
	svc := &fakeMerchantConsoleService{
		trend: &service.MerchantUsageTrend{
			Days:      7,
			StartDate: "2026-10-24",
			EndDate:   "2026-10-30",
			Points: []service.MerchantDailyUsagePoint{
				{Date: "2026-10-30", Requests: 3, GrossCost: 2.5, Commission: 0.2, NetPayable: 2.3},
			},
		},
	}
	id := int64(5)
	r := newMerchantConsoleTestRouter(t, svc, &id)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/merchant/usage/daily", nil))

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, 7, svc.gotDays)
	require.Equal(t, int64(5), svc.gotMerchantID)

	var body struct {
		Data struct {
			Days      int    `json:"days"`
			StartDate string `json:"start_date"`
			EndDate   string `json:"end_date"`
			Items     []struct {
				Date       string  `json:"date"`
				Requests   int64   `json:"requests"`
				GrossCost  float64 `json:"gross_cost"`
				Commission float64 `json:"commission"`
				NetPayable float64 `json:"net_payable"`
			} `json:"items"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Equal(t, 7, body.Data.Days)
	require.Equal(t, "2026-10-24", body.Data.StartDate)
	require.Equal(t, "2026-10-30", body.Data.EndDate)
	require.Len(t, body.Data.Items, 1)
	require.Equal(t, int64(3), body.Data.Items[0].Requests)
	require.InDelta(t, 2.3, body.Data.Items[0].NetPayable, 1e-9)
}

func TestMerchantConsoleDailyUsageRejectsInvalidDays(t *testing.T) {
	id := int64(5)
	r := newMerchantConsoleTestRouter(t, &fakeMerchantConsoleService{}, &id)

	for _, query := range []string{"?days=abc", "?days=0", "?days=-1"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/merchant/usage/daily"+query, nil))
		require.Equal(t, http.StatusBadRequest, w.Code, query)
	}
}
