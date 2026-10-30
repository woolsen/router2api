package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/woolsen/router2api/internal/server/middleware"
	"github.com/woolsen/router2api/internal/service"
)

type fakeMerchantWithdrawalService struct {
	available float64
	items     []service.Withdrawal
	err       error

	gotMerchantID int64
	gotAmount     float64
	calls         int
}

func (f *fakeMerchantWithdrawalService) Available(_ context.Context, merchantID int64) (float64, error) {
	f.calls++
	f.gotMerchantID = merchantID
	return f.available, f.err
}

func (f *fakeMerchantWithdrawalService) List(_ context.Context, merchantID int64) ([]service.Withdrawal, error) {
	f.calls++
	f.gotMerchantID = merchantID
	return f.items, f.err
}

func (f *fakeMerchantWithdrawalService) Request(_ context.Context, merchantID int64, amount float64) (*service.Withdrawal, error) {
	f.calls++
	f.gotMerchantID = merchantID
	f.gotAmount = amount
	if f.err != nil {
		return nil, f.err
	}
	return &service.Withdrawal{ID: 1, MerchantID: merchantID, Amount: amount, Status: service.WithdrawalStatusRequested}, nil
}

func newMerchantWithdrawalTestRouter(svc merchantWithdrawalService, merchantID *int64) *gin.Engine {
	gin.SetMode(gin.TestMode)
	h := &MerchantWithdrawalHandler{withdrawalService: svc}
	r := gin.New()
	r.Use(func(c *gin.Context) {
		if merchantID != nil {
			c.Set(string(middleware.ContextKeyMerchantID), *merchantID)
		}
		c.Next()
	})
	r.GET("/api/v1/merchant/withdrawals", h.List)
	r.POST("/api/v1/merchant/withdrawals", h.Request)
	return r
}

func TestMerchantWithdrawalListUsesAuthenticatedScope(t *testing.T) {
	svc := &fakeMerchantWithdrawalService{
		available: 123.45,
		items: []service.Withdrawal{
			{ID: 1, MerchantID: 5, Amount: 60, Status: service.WithdrawalStatusRequested},
		},
	}
	id := int64(5)
	r := newMerchantWithdrawalTestRouter(svc, &id)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/merchant/withdrawals?merchant_id=999", nil))

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, int64(5), svc.gotMerchantID, "query merchant_id must be ignored")

	var body struct {
		Data struct {
			Available float64              `json:"available"`
			Minimum   float64              `json:"minimum"`
			Items     []withdrawalResponse `json:"items"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.InDelta(t, 123.45, body.Data.Available, 1e-9)
	require.InDelta(t, service.WithdrawalMinimumAmount, body.Data.Minimum, 1e-9)
	require.Len(t, body.Data.Items, 1)
}

func TestMerchantWithdrawalRequestIgnoresBodyMerchantID(t *testing.T) {
	svc := &fakeMerchantWithdrawalService{}
	id := int64(5)
	r := newMerchantWithdrawalTestRouter(svc, &id)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/merchant/withdrawals",
		strings.NewReader(`{"amount":60,"merchant_id":999}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusCreated, w.Code)
	require.Equal(t, int64(5), svc.gotMerchantID, "body merchant_id must be ignored")
	require.InDelta(t, 60, svc.gotAmount, 1e-9)
}

func TestMerchantWithdrawalRejectsMissingScope(t *testing.T) {
	svc := &fakeMerchantWithdrawalService{}
	r := newMerchantWithdrawalTestRouter(svc, nil)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/merchant/withdrawals", nil))

	require.Equal(t, http.StatusForbidden, w.Code)
	require.Contains(t, w.Body.String(), `"reason":"MERCHANT_ONLY"`)
	require.Zero(t, svc.calls)
}
