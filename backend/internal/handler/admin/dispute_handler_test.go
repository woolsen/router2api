package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/woolsen/router2api/internal/server/middleware"
	"github.com/woolsen/router2api/internal/service"
)

type fakeAdminDisputeService struct {
	err error

	items []service.Dispute

	gotStatus    string
	gotID        int64
	gotIn        service.ResolveInput
	listCalls    int
	resolveCalls int
}

func (f *fakeAdminDisputeService) ListAll(_ context.Context, status string) ([]service.Dispute, error) {
	f.listCalls++
	f.gotStatus = status
	return f.items, f.err
}

func (f *fakeAdminDisputeService) Resolve(_ context.Context, id int64, in service.ResolveInput) (*service.Dispute, error) {
	f.resolveCalls++
	f.gotID = id
	f.gotIn = in
	if f.err != nil {
		return nil, f.err
	}
	return &service.Dispute{ID: id, Status: service.DisputeStatusResolved,
		Decision: in.Decision, AdjustAmount: in.AdjustAmount,
		Note: in.Note, ResolvedBy: in.By}, nil
}

func newAdminDisputeTestRouter(svc adminDisputeService) *gin.Engine {
	gin.SetMode(gin.TestMode)
	h := &DisputeHandler{disputeService: svc}
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(middleware.ContextKeyAuthEmail, "ops@example.com")
		c.Next()
	})
	r.GET("/api/v1/admin/disputes", h.List)
	r.POST("/api/v1/admin/disputes/:id/resolve", h.Resolve)
	return r
}

func TestDisputeAdminListForwardsStatus(t *testing.T) {
	svc := &fakeAdminDisputeService{items: []service.Dispute{{ID: 1, Status: service.DisputeStatusOpen}}}
	r := newAdminDisputeTestRouter(svc)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/admin/disputes?status=open", nil))

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "open", svc.gotStatus)
	require.Contains(t, w.Body.String(), `"items"`)
}

func TestDisputeAdminResolvePassesDecisionAndOperator(t *testing.T) {
	svc := &fakeAdminDisputeService{}
	r := newAdminDisputeTestRouter(svc)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/disputes/7/resolve",
		strings.NewReader(`{"decision":"adjust","adjust_amount":-3,"note":"核对误差"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, int64(7), svc.gotID)
	require.Equal(t, service.DisputeDecisionAdjust, svc.gotIn.Decision)
	require.InDelta(t, -3, svc.gotIn.AdjustAmount, 1e-9)
	require.Equal(t, "核对误差", svc.gotIn.Note)
	require.Equal(t, "ops@example.com", svc.gotIn.By)
}

func TestDisputeAdminResolveRejectsInvalidID(t *testing.T) {
	svc := &fakeAdminDisputeService{}
	r := newAdminDisputeTestRouter(svc)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/disputes/0/resolve",
		strings.NewReader(`{"decision":"reject"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Zero(t, svc.resolveCalls)
}

func TestDisputeAdminResolveReturnsConflict(t *testing.T) {
	svc := &fakeAdminDisputeService{err: service.ErrDisputeWindowClosed}
	r := newAdminDisputeTestRouter(svc)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/disputes/7/resolve",
		strings.NewReader(`{"decision":"confirm"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusConflict, w.Code)
	require.Contains(t, w.Body.String(), `"reason":"DISPUTE_WINDOW_CLOSED"`)
}
