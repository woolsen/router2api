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

type fakeMerchantDisputeService struct {
	items []service.Dispute
	err   error

	gotOpen  service.OpenDisputeInput
	gotList  int64
	openCall int
	listCall int
}

func (f *fakeMerchantDisputeService) Open(_ context.Context, in service.OpenDisputeInput) (*service.Dispute, error) {
	f.openCall++
	f.gotOpen = in
	if f.err != nil {
		return nil, f.err
	}
	return &service.Dispute{
		ID: 1, OwnerType: in.OwnerType, OwnerID: in.OwnerID,
		TargetType: in.TargetType, TargetID: in.TargetID,
		Amount: in.Amount, Reason: in.Reason, Status: service.DisputeStatusOpen,
	}, nil
}

func (f *fakeMerchantDisputeService) ListByMerchant(_ context.Context, merchantID int64) ([]service.Dispute, error) {
	f.listCall++
	f.gotList = merchantID
	return f.items, f.err
}

func newMerchantDisputeTestRouter(svc merchantDisputeService, merchantID *int64) *gin.Engine {
	gin.SetMode(gin.TestMode)
	h := &MerchantDisputeHandler{disputeService: svc}
	r := gin.New()
	r.Use(func(c *gin.Context) {
		if merchantID != nil {
			c.Set(string(middleware.ContextKeyMerchantID), *merchantID)
		}
		c.Next()
	})
	r.GET("/api/v1/merchant/disputes", h.List)
	r.POST("/api/v1/merchant/disputes", h.Open)
	return r
}

func TestDisputeMerchantOpenUsesAuthenticatedScope(t *testing.T) {
	svc := &fakeMerchantDisputeService{}
	merchantID := int64(5)
	r := newMerchantDisputeTestRouter(svc, &merchantID)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/merchant/disputes",
		strings.NewReader(`{"settlement_id":9,"amount":3,"reason":"usage high","merchant_id":999,"owner_id":999}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusCreated, w.Code)
	require.Equal(t, service.DisputeOwnerMerchant, svc.gotOpen.OwnerType)
	require.Equal(t, int64(5), svc.gotOpen.OwnerID, "body merchant_id must be ignored")
	require.Equal(t, service.DisputeTargetSettlement, svc.gotOpen.TargetType)
	require.Equal(t, int64(9), svc.gotOpen.TargetID)

	var body struct {
		Data disputeResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Equal(t, int64(1), body.Data.ID)
	require.Equal(t, service.DisputeStatusOpen, body.Data.Status)
}

func TestDisputeMerchantListUsesAuthenticatedScope(t *testing.T) {
	svc := &fakeMerchantDisputeService{items: []service.Dispute{
		{ID: 1, OwnerType: service.DisputeOwnerMerchant, OwnerID: 5,
			TargetType: service.DisputeTargetSettlement, TargetID: 9,
			Status: service.DisputeStatusOpen},
	}}
	merchantID := int64(5)
	r := newMerchantDisputeTestRouter(svc, &merchantID)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/merchant/disputes?merchant_id=999", nil))

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, int64(5), svc.gotList)
}

func TestDisputeMerchantRequiresScope(t *testing.T) {
	svc := &fakeMerchantDisputeService{}
	r := newMerchantDisputeTestRouter(svc, nil)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/merchant/disputes", nil))

	require.Equal(t, http.StatusForbidden, w.Code)
	require.Contains(t, w.Body.String(), `"reason":"MERCHANT_ONLY"`)
	require.Zero(t, svc.listCall)
}

func TestDisputeMerchantOpenRejectsClosedWindow(t *testing.T) {
	svc := &fakeMerchantDisputeService{err: service.ErrDisputeWindowClosed}
	merchantID := int64(5)
	r := newMerchantDisputeTestRouter(svc, &merchantID)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/merchant/disputes",
		strings.NewReader(`{"settlement_id":9,"amount":3,"reason":"late"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusConflict, w.Code)
	require.Contains(t, w.Body.String(), `"reason":"DISPUTE_WINDOW_CLOSED"`)
}
