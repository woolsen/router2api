package handler

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

type fakeUserDisputeService struct {
	gotOpen  service.OpenDisputeInput
	gotList  int64
	openCall int
}

func (f *fakeUserDisputeService) Open(_ context.Context, in service.OpenDisputeInput) (*service.Dispute, error) {
	f.openCall++
	f.gotOpen = in
	return &service.Dispute{ID: 1, OwnerType: in.OwnerType, OwnerID: in.OwnerID,
		TargetType: in.TargetType, TargetID: in.TargetID, Status: service.DisputeStatusOpen}, nil
}

func (f *fakeUserDisputeService) ListByUser(_ context.Context, userID int64) ([]service.Dispute, error) {
	f.gotList = userID
	return nil, nil
}

func newUserDisputeTestRouter(svc userDisputeService, userID *int64) *gin.Engine {
	gin.SetMode(gin.TestMode)
	h := &UserDisputeHandler{disputeService: svc}
	r := gin.New()
	r.Use(func(c *gin.Context) {
		if userID != nil {
			c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: *userID})
		}
		c.Next()
	})
	r.GET("/api/v1/disputes", h.List)
	r.POST("/api/v1/disputes", h.Open)
	return r
}

func TestDisputeUserOpenUsesAuthenticatedUser(t *testing.T) {
	svc := &fakeUserDisputeService{}
	userID := int64(42)
	r := newUserDisputeTestRouter(svc, &userID)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/disputes",
		strings.NewReader(`{"usage_log_id":99,"amount":0.5,"reason":"double charged","user_id":999,"owner_id":999}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusCreated, w.Code)
	require.Equal(t, service.DisputeOwnerUser, svc.gotOpen.OwnerType)
	require.Equal(t, int64(42), svc.gotOpen.OwnerID, "body user_id must be ignored")
	require.Equal(t, service.DisputeTargetUsage, svc.gotOpen.TargetType)
	require.Equal(t, int64(99), svc.gotOpen.TargetID)
}

func TestDisputeUserListUsesAuthenticatedUser(t *testing.T) {
	svc := &fakeUserDisputeService{}
	userID := int64(42)
	r := newUserDisputeTestRouter(svc, &userID)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/disputes?user_id=999", nil))

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, int64(42), svc.gotList)
}

func TestDisputeUserRequiresAuthentication(t *testing.T) {
	svc := &fakeUserDisputeService{}
	r := newUserDisputeTestRouter(svc, nil)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/disputes",
		strings.NewReader(`{"usage_log_id":99,"reason":"x"}`)))

	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Zero(t, svc.openCall)
}
