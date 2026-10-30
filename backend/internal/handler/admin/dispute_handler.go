package admin

import (
	"context"
	"strconv"
	"time"

	"github.com/woolsen/router2api/internal/pkg/response"
	"github.com/woolsen/router2api/internal/service"

	"github.com/gin-gonic/gin"
)

// DisputeHandler 是运营端申诉仲裁接口。
// 路由挂在 /admin 下，只对 admin 角色开放。
type DisputeHandler struct {
	disputeService adminDisputeService
}

// adminDisputeService 是处理器依赖的最小用例集合，便于用假实现做接口测试。
type adminDisputeService interface {
	ListAll(ctx context.Context, status string) ([]service.Dispute, error)
	Resolve(ctx context.Context, id int64, in service.ResolveInput) (*service.Dispute, error)
}

func NewDisputeHandler(disputeService *service.DisputeService) *DisputeHandler {
	return &DisputeHandler{disputeService: disputeService}
}

type adminDisputeResponse struct {
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

type adminDisputeListResponse struct {
	Items []adminDisputeResponse `json:"items"`
}

func adminDisputeResponseFrom(d *service.Dispute) adminDisputeResponse {
	out := adminDisputeResponse{}
	if d == nil {
		return out
	}
	out.ID = d.ID
	out.OwnerType = d.OwnerType
	out.OwnerID = d.OwnerID
	out.TargetType = d.TargetType
	out.TargetID = d.TargetID
	out.Amount = d.Amount
	out.Reason = d.Reason
	out.Status = d.Status
	out.Decision = d.Decision
	out.AdjustAmount = d.AdjustAmount
	out.Note = d.Note
	out.ResolvedBy = d.ResolvedBy
	out.CreatedAt = d.CreatedAt
	out.ResolvedAt = d.ResolvedAt
	return out
}

type resolveDisputeRequest struct {
	Decision     string  `json:"decision"`
	AdjustAmount float64 `json:"adjust_amount"`
	Note         string  `json:"note"`
}

// List 返回运营端申诉列表，可按 status=open|resolved 过滤。
// GET /api/v1/admin/disputes
func (h *DisputeHandler) List(c *gin.Context) {
	items, err := h.disputeService.ListAll(c.Request.Context(), c.Query("status"))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	out := make([]adminDisputeResponse, 0, len(items))
	for i := range items {
		out = append(out, adminDisputeResponseFrom(&items[i]))
	}
	response.Success(c, adminDisputeListResponse{Items: out})
}

// Resolve 裁决一条 open 申诉。
// POST /api/v1/admin/disputes/:id/resolve
func (h *DisputeHandler) Resolve(c *gin.Context) {
	id, ok := parseDisputeID(c)
	if !ok {
		return
	}

	var req resolveDisputeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	d, err := h.disputeService.Resolve(c.Request.Context(), id, service.ResolveInput{
		Decision:     req.Decision,
		AdjustAmount: req.AdjustAmount,
		Note:         req.Note,
		By:           withdrawalOperator(c),
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, adminDisputeResponseFrom(d))
}

func parseDisputeID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid dispute ID")
		return 0, false
	}
	return id, true
}
