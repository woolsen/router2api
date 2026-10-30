package handler

import (
	"context"
	"time"

	"github.com/woolsen/router2api/internal/pkg/response"
	"github.com/woolsen/router2api/internal/service"

	"github.com/gin-gonic/gin"
)

// MerchantDisputeHandler 提供商户申诉接口。
// merchant_id 只来自 MerchantScopeMiddleware，处理器不读取 query/body 中的商户身份。
type MerchantDisputeHandler struct {
	disputeService merchantDisputeService
}

// merchantDisputeService 是处理器依赖的最小用例集合，便于用假实现做接口测试。
type merchantDisputeService interface {
	Open(ctx context.Context, in service.OpenDisputeInput) (*service.Dispute, error)
	ListByMerchant(ctx context.Context, merchantID int64) ([]service.Dispute, error)
}

func NewMerchantDisputeHandler(disputeService *service.DisputeService) *MerchantDisputeHandler {
	return &MerchantDisputeHandler{disputeService: disputeService}
}

type disputeResponse struct {
	ID           int64      `json:"id"`
	OwnerType    string     `json:"owner_type"`
	OwnerID      int64      `json:"owner_id"`
	TargetType   string     `json:"target_type"`
	TargetID     int64      `json:"target_id"`
	Amount       float64    `json:"amount"`
	Reason       string     `json:"reason"`
	Status       string     `json:"status"`
	Decision     string     `json:"decision,omitempty"`
	AdjustAmount float64    `json:"adjust_amount"`
	Note         string     `json:"note,omitempty"`
	ResolvedBy   string     `json:"resolved_by,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	ResolvedAt   *time.Time `json:"resolved_at,omitempty"`
}

func disputeResponseFrom(d *service.Dispute) disputeResponse {
	if d == nil {
		return disputeResponse{}
	}
	return disputeResponse{
		ID:           d.ID,
		OwnerType:    d.OwnerType,
		OwnerID:      d.OwnerID,
		TargetType:   d.TargetType,
		TargetID:     d.TargetID,
		Amount:       d.Amount,
		Reason:       d.Reason,
		Status:       d.Status,
		Decision:     d.Decision,
		AdjustAmount: d.AdjustAmount,
		Note:         d.Note,
		ResolvedBy:   d.ResolvedBy,
		CreatedAt:    d.CreatedAt,
		ResolvedAt:   d.ResolvedAt,
	}
}

type disputeListResponse struct {
	Items []disputeResponse `json:"items"`
}

type openSettlementDisputeRequest struct {
	SettlementID int64   `json:"settlement_id"`
	Amount       float64 `json:"amount"`
	Reason       string  `json:"reason"`
}

// List 返回当前商户的申诉记录。
// GET /api/v1/merchant/disputes
func (h *MerchantDisputeHandler) List(c *gin.Context) {
	merchantID, ok := requireMerchantScope(c)
	if !ok {
		return
	}

	items, err := h.disputeService.ListByMerchant(c.Request.Context(), merchantID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	out := make([]disputeResponse, 0, len(items))
	for i := range items {
		out = append(out, disputeResponseFrom(&items[i]))
	}
	response.Success(c, disputeListResponse{Items: out})
}

// Open 发起针对本商户结算单的申诉。
// POST /api/v1/merchant/disputes
func (h *MerchantDisputeHandler) Open(c *gin.Context) {
	merchantID, ok := requireMerchantScope(c)
	if !ok {
		return
	}

	var req openSettlementDisputeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	d, err := h.disputeService.Open(c.Request.Context(), service.OpenDisputeInput{
		OwnerType:  service.DisputeOwnerMerchant,
		OwnerID:    merchantID,
		TargetType: service.DisputeTargetSettlement,
		TargetID:   req.SettlementID,
		Amount:     req.Amount,
		Reason:     req.Reason,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Created(c, disputeResponseFrom(d))
}
