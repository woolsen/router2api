package admin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/woolsen/router2api/internal/server/middleware"
	"github.com/woolsen/router2api/internal/service"
)

type fakeAdminWithdrawalService struct {
	err error

	markPaidCalled bool
	gotID          int64
	gotProofPath   string
	gotSHA256      string
	gotBy          string
	approveBy      string
	rejectBy       string
}

func (f *fakeAdminWithdrawalService) ListAll(context.Context, string) ([]service.Withdrawal, error) {
	return nil, f.err
}

func (f *fakeAdminWithdrawalService) Approve(_ context.Context, _ int64, by string) error {
	f.approveBy = by
	return f.err
}

func (f *fakeAdminWithdrawalService) Reject(_ context.Context, _ int64, by string) error {
	f.rejectBy = by
	return f.err
}

func (f *fakeAdminWithdrawalService) MarkPaid(_ context.Context, id int64, proofPath, sha256, by string) error {
	f.markPaidCalled = true
	f.gotID = id
	f.gotProofPath = proofPath
	f.gotSHA256 = sha256
	f.gotBy = by
	return f.err
}

func newAdminWithdrawalTestRouter(t *testing.T, svc adminWithdrawalService) *gin.Engine {
	t.Helper()
	t.Setenv("DATA_DIR", t.TempDir())
	gin.SetMode(gin.TestMode)
	h := &WithdrawalHandler{withdrawalService: svc, proofDir: resolveWithdrawalProofDir()}
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(middleware.ContextKeyAuthEmail, "ops@example.com")
		c.Next()
	})
	r.POST("/api/v1/admin/withdrawals/:id/pay", h.Pay)
	r.POST("/api/v1/admin/withdrawals/:id/approve", h.Approve)
	r.POST("/api/v1/admin/withdrawals/:id/reject", h.Reject)
	return r
}

func multipartProofBody(t *testing.T, content string) (*bytes.Buffer, string) {
	t.Helper()
	buf := &bytes.Buffer{}
	writer := multipart.NewWriter(buf)
	part, err := writer.CreateFormFile("proof", "proof.png")
	require.NoError(t, err)
	_, err = part.Write([]byte(content))
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	return buf, writer.FormDataContentType()
}

func TestAdminWithdrawalPayStoresProofAndHashesIt(t *testing.T) {
	svc := &fakeAdminWithdrawalService{}
	r := newAdminWithdrawalTestRouter(t, svc)

	body, contentType := multipartProofBody(t, "payment-proof")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/withdrawals/11/pay", body)
	req.Header.Set("Content-Type", contentType)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.True(t, svc.markPaidCalled)
	require.Equal(t, int64(11), svc.gotID)
	require.Equal(t, "ops@example.com", svc.gotBy)

	sum := sha256.Sum256([]byte("payment-proof"))
	require.Equal(t, hex.EncodeToString(sum[:]), svc.gotSHA256)
	require.FileExists(t, svc.gotProofPath)
	stored, err := os.ReadFile(svc.gotProofPath)
	require.NoError(t, err)
	require.Equal(t, "payment-proof", string(stored))
	t.Cleanup(func() { _ = os.Remove(svc.gotProofPath) })
}

func TestAdminWithdrawalPayRequiresProofFile(t *testing.T) {
	svc := &fakeAdminWithdrawalService{}
	r := newAdminWithdrawalTestRouter(t, svc)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/withdrawals/11/pay", bytes.NewBufferString(""))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=missing")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code)
	require.False(t, svc.markPaidCalled)
}

func TestAdminWithdrawalApproveUsesAuthenticatedOperator(t *testing.T) {
	svc := &fakeAdminWithdrawalService{}
	r := newAdminWithdrawalTestRouter(t, svc)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/admin/withdrawals/11/approve", nil))

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "ops@example.com", svc.approveBy)
}
