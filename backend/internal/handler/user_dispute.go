package handler

import (
	"context"

	"github.com/woolsen/router2api/internal/pkg/response"
	"github.com/woolsen/router2api/internal/server/middleware"
	"github.com/woolsen/router2api/internal/service"

	"github.com/gin-gonic/gin"
)

// UserDisputeHandler 提供普通用户对单笔扣费的申诉接口。
// 用户身份只来自认证中间件；目标 usage_logs 的归属由仓储校验。
type UserDisputeHandler struct {
	disputeService userDisputeService
}

type userDisputeService interface {
	Open(ctx context.Context, in service.OpenDisputeInput) (*service.Dispute, error)
	ListByUser(ctx context.Context, userID int64) ([]service.Dispute, error)
}

func NewUserDisputeHandler(disputeService *service.DisputeService) *UserDisputeHandler {
	return &UserDisputeHandler{disputeService: disputeService}
}

type openUsageDisputeRequest struct {
	UsageLogID int64   `json:"usage_log_id"`
	Amount     float64 `json:"amount"`
	Reason     string  `json:"reason"`
}

// List 返回当前用户的申诉记录。
// GET /api/v1/disputes
func (h *UserDisputeHandler) List(c *gin.Context) {
	userID, ok := requireAuthUser(c)
	if !ok {
		return
	}

	items, err := h.disputeService.ListByUser(c.Request.Context(), userID)
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

// Open 发起针对单条用量记录的申诉。
// POST /api/v1/disputes
func (h *UserDisputeHandler) Open(c *gin.Context) {
	userID, ok := requireAuthUser(c)
	if !ok {
		return
	}

	var req openUsageDisputeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	d, err := h.disputeService.Open(c.Request.Context(), service.OpenDisputeInput{
		OwnerType:  service.DisputeOwnerUser,
		OwnerID:    userID,
		TargetType: service.DisputeTargetUsage,
		TargetID:   req.UsageLogID,
		Amount:     req.Amount,
		Reason:     req.Reason,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Created(c, disputeResponseFrom(d))
}

func requireAuthUser(c *gin.Context) (int64, bool) {
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok || subject.UserID <= 0 {
		response.Unauthorized(c, "User not authenticated")
		return 0, false
	}
	return subject.UserID, true
}
