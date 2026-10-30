package handler

import (
	"context"
	"time"

	"github.com/woolsen/router2api/internal/pkg/response"
	"github.com/woolsen/router2api/internal/service"

	"github.com/gin-gonic/gin"
)

// MerchantWithdrawalHandler 提供商户钱包与提现自助接口。
// merchant_id 只来自 MerchantScopeMiddleware，处理器不读取 query/body 中的商户身份。
type MerchantWithdrawalHandler struct {
	withdrawalService merchantWithdrawalService
}

// merchantWithdrawalService 是处理器依赖的最小用例集合，便于用假实现做接口测试。
type merchantWithdrawalService interface {
	Available(ctx context.Context, merchantID int64) (float64, error)
	List(ctx context.Context, merchantID int64) ([]service.Withdrawal, error)
	Request(ctx context.Context, merchantID int64, amount float64) (*service.Withdrawal, error)
}

func NewMerchantWithdrawalHandler(withdrawalService *service.WithdrawalService) *MerchantWithdrawalHandler {
	return &MerchantWithdrawalHandler{withdrawalService: withdrawalService}
}

type withdrawalResponse struct {
	ID          int64      `json:"id"`
	MerchantID  int64      `json:"merchant_id"`
	Amount      float64    `json:"amount"`
	Status      string     `json:"status"`
	RequestedAt time.Time  `json:"requested_at"`
	PaidAt      *time.Time `json:"paid_at,omitempty"`
}

func withdrawalResponseFrom(w *service.Withdrawal) withdrawalResponse {
	if w == nil {
		return withdrawalResponse{}
	}
	return withdrawalResponse{
		ID:          w.ID,
		MerchantID:  w.MerchantID,
		Amount:      w.Amount,
		Status:      w.Status,
		RequestedAt: w.RequestedAt,
		PaidAt:      w.PaidAt,
	}
}

type merchantWithdrawalsResponse struct {
	Available float64              `json:"available"`
	Minimum   float64              `json:"minimum"`
	Items     []withdrawalResponse `json:"items"`
}

type requestWithdrawalRequest struct {
	Amount float64 `json:"amount"`
}

// List 返回当前商户的可提现余额与提现记录。
// GET /api/v1/merchant/withdrawals
func (h *MerchantWithdrawalHandler) List(c *gin.Context) {
	merchantID, ok := requireMerchantScope(c)
	if !ok {
		return
	}

	available, err := h.withdrawalService.Available(c.Request.Context(), merchantID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	items, err := h.withdrawalService.List(c.Request.Context(), merchantID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	out := make([]withdrawalResponse, 0, len(items))
	for i := range items {
		out = append(out, withdrawalResponseFrom(&items[i]))
	}
	response.Success(c, merchantWithdrawalsResponse{
		Available: available,
		Minimum:   service.WithdrawalMinimumAmount,
		Items:     out,
	})
}

// Request 提交一笔提现申请。金额只来自 body；商户身份只来自认证中间件。
// POST /api/v1/merchant/withdrawals
func (h *MerchantWithdrawalHandler) Request(c *gin.Context) {
	merchantID, ok := requireMerchantScope(c)
	if !ok {
		return
	}

	var req requestWithdrawalRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	w, err := h.withdrawalService.Request(c.Request.Context(), merchantID, req.Amount)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Created(c, withdrawalResponseFrom(w))
}
