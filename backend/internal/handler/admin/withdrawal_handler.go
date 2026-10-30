package admin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/woolsen/router2api/internal/pkg/response"
	"github.com/woolsen/router2api/internal/server/middleware"
	"github.com/woolsen/router2api/internal/service"

	"github.com/gin-gonic/gin"
)

const (
	withdrawalProofMaxBytes = 10 << 20 // 10 MiB
	withdrawalProofField    = "proof"
)

// WithdrawalHandler 是运营端提现审核与打款接口。
type WithdrawalHandler struct {
	withdrawalService adminWithdrawalService
	proofDir          string
}

// adminWithdrawalService 是处理器依赖的最小用例集合，便于用假实现做接口测试。
type adminWithdrawalService interface {
	ListAll(ctx context.Context, status string) ([]service.Withdrawal, error)
	Approve(ctx context.Context, withdrawalID int64, by string) error
	Reject(ctx context.Context, withdrawalID int64, by string) error
	MarkPaid(ctx context.Context, withdrawalID int64, proofPath, sha256, by string) error
}

func NewWithdrawalHandler(withdrawalService *service.WithdrawalService) *WithdrawalHandler {
	return &WithdrawalHandler{
		withdrawalService: withdrawalService,
		proofDir:          resolveWithdrawalProofDir(),
	}
}

// resolveWithdrawalProofDir 与插件安装目录一致：DATA_DIR 优先，默认 ./data。
func resolveWithdrawalProofDir() string {
	base := strings.TrimSpace(os.Getenv("DATA_DIR"))
	if base == "" {
		base = "./data"
	}
	return filepath.Join(base, "withdrawal-proofs")
}

type adminWithdrawalResponse struct {
	ID          int64      `json:"id"`
	MerchantID  int64      `json:"merchant_id"`
	Amount      float64    `json:"amount"`
	Status      string     `json:"status"`
	ProofPath   string     `json:"proof_path,omitempty"`
	ProofSHA256 string     `json:"proof_sha256,omitempty"`
	ApprovedBy  string     `json:"approved_by,omitempty"`
	RequestedAt time.Time  `json:"requested_at"`
	PaidAt      *time.Time `json:"paid_at,omitempty"`
}

type adminWithdrawalsResponse struct {
	Items []adminWithdrawalResponse `json:"items"`
}

// List 返回运营端提现单，可按 status 过滤。
// GET /api/v1/admin/withdrawals?status=requested
func (h *WithdrawalHandler) List(c *gin.Context) {
	items, err := h.withdrawalService.ListAll(c.Request.Context(), c.Query("status"))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	out := make([]adminWithdrawalResponse, 0, len(items))
	for i := range items {
		w := &items[i]
		out = append(out, adminWithdrawalResponse{
			ID:          w.ID,
			MerchantID:  w.MerchantID,
			Amount:      w.Amount,
			Status:      w.Status,
			ProofPath:   w.ProofPath,
			ProofSHA256: w.ProofSHA256,
			ApprovedBy:  w.ApprovedBy,
			RequestedAt: w.RequestedAt,
			PaidAt:      w.PaidAt,
		})
	}
	response.Success(c, adminWithdrawalsResponse{Items: out})
}

// Approve 审核通过一笔提现，等待线下打款。
// POST /api/v1/admin/withdrawals/:id/approve
func (h *WithdrawalHandler) Approve(c *gin.Context) {
	id, ok := parseWithdrawalID(c)
	if !ok {
		return
	}
	if err := h.withdrawalService.Approve(c.Request.Context(), id, withdrawalOperator(c)); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"id": id, "status": service.WithdrawalStatusApproved})
}

// Reject 驳回提现并解冻金额。
// POST /api/v1/admin/withdrawals/:id/reject
func (h *WithdrawalHandler) Reject(c *gin.Context) {
	id, ok := parseWithdrawalID(c)
	if !ok {
		return
	}
	if err := h.withdrawalService.Reject(c.Request.Context(), id, withdrawalOperator(c)); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"id": id, "status": service.WithdrawalStatusRejected})
}

// Pay 上传打款凭证并完成打款。
// POST /api/v1/admin/withdrawals/:id/pay  (multipart/form-data, field "proof")
func (h *WithdrawalHandler) Pay(c *gin.Context) {
	id, ok := parseWithdrawalID(c)
	if !ok {
		return
	}

	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, withdrawalProofMaxBytes+(1<<20))
	file, header, err := c.Request.FormFile(withdrawalProofField)
	if err != nil {
		response.BadRequest(c, "proof file is required (multipart field: proof)")
		return
	}
	defer func() { _ = file.Close() }()

	proofPath, checksum, err := h.saveProof(id, header.Filename, file)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	if err := h.withdrawalService.MarkPaid(c.Request.Context(), id, proofPath, checksum, withdrawalOperator(c)); err != nil {
		// 打款状态没推进时不能留下孤立凭证，避免同一单出现两份文件。
		_ = os.Remove(proofPath)
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{
		"id":           id,
		"status":       service.WithdrawalStatusPaid,
		"proof_path":   proofPath,
		"proof_sha256": checksum,
	})
}

func (h *WithdrawalHandler) saveProof(withdrawalID int64, originalName string, file io.Reader) (string, string, error) {
	if err := os.MkdirAll(h.proofDir, 0o750); err != nil {
		return "", "", fmt.Errorf("create withdrawal proof dir: %w", err)
	}

	ext := strings.ToLower(filepath.Ext(filepath.Base(originalName)))
	if len(ext) > 10 {
		ext = ""
	}
	name := fmt.Sprintf("withdrawal-%d-%d%s", withdrawalID, time.Now().UTC().UnixNano(), ext)
	path := filepath.Join(h.proofDir, name)

	out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return "", "", fmt.Errorf("create withdrawal proof: %w", err)
	}

	hash := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(out, hash), io.LimitReader(file, withdrawalProofMaxBytes+1))
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(path)
		return "", "", fmt.Errorf("save withdrawal proof: %w", copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(path)
		return "", "", fmt.Errorf("close withdrawal proof: %w", closeErr)
	}
	if info, err := os.Stat(path); err != nil || info.Size() > withdrawalProofMaxBytes {
		_ = os.Remove(path)
		return "", "", fmt.Errorf("withdrawal proof exceeds %d bytes", withdrawalProofMaxBytes)
	}

	return path, hex.EncodeToString(hash.Sum(nil)), nil
}

func parseWithdrawalID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid withdrawal ID")
		return 0, false
	}
	return id, true
}

func withdrawalOperator(c *gin.Context) string {
	if email := strings.TrimSpace(c.GetString(middleware.ContextKeyAuthEmail)); email != "" {
		return email
	}
	if subject, ok := middleware.GetAuthSubjectFromContext(c); ok && subject.UserID > 0 {
		return fmt.Sprintf("admin:%d", subject.UserID)
	}
	return service.SettlementDefaultOperator
}
