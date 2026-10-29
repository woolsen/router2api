package admin

import (
	"errors"
	"strconv"

	"github.com/woolsen/router2api/internal/pkg/pagination"
	"github.com/woolsen/router2api/internal/pkg/response"
	"github.com/woolsen/router2api/internal/service"

	"github.com/gin-gonic/gin"
)

// MerchantHandler 管理入驻商户（中转站）的后台 CRUD。
type MerchantHandler struct {
	merchantService *service.MerchantService
}

func NewMerchantHandler(merchantService *service.MerchantService) *MerchantHandler {
	return &MerchantHandler{merchantService: merchantService}
}

type createMerchantRequest struct {
	Name           string   `json:"name" binding:"required"`
	Contact        string   `json:"contact"`
	CommissionRate *float64 `json:"commission_rate"`
}

type updateMerchantCommissionRequest struct {
	CommissionRate *float64 `json:"commission_rate" binding:"required"`
}

type merchantResponse struct {
	ID             int64   `json:"id"`
	Name           string  `json:"name"`
	Contact        string  `json:"contact"`
	Status         string  `json:"status"`
	CommissionRate float64 `json:"commission_rate"`
}

func merchantResponseFromService(m *service.Merchant) *merchantResponse {
	if m == nil {
		return nil
	}
	return &merchantResponse{
		ID:             m.ID,
		Name:           m.Name,
		Contact:        m.Contact,
		Status:         m.Status,
		CommissionRate: m.CommissionRate,
	}
}

// Create 创建商户。
// POST /api/v1/admin/merchants
func (h *MerchantHandler) Create(c *gin.Context) {
	var req createMerchantRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	created, err := h.merchantService.Create(c.Request.Context(), service.CreateMerchantInput{
		Name:           req.Name,
		Contact:        req.Contact,
		CommissionRate: req.CommissionRate,
	})
	if err != nil {
		h.respondError(c, err)
		return
	}
	response.Success(c, merchantResponseFromService(created))
}

// List 分页列出商户。
// GET /api/v1/admin/merchants
func (h *MerchantHandler) List(c *gin.Context) {
	page, pageSize := response.ParsePagination(c)
	params := pagination.PaginationParams{
		Page:      page,
		PageSize:  pageSize,
		SortBy:    "id",
		SortOrder: pagination.SortOrderDesc,
	}

	items, result, err := h.merchantService.List(c.Request.Context(), params)
	if err != nil {
		h.respondError(c, err)
		return
	}

	out := make([]*merchantResponse, 0, len(items))
	for i := range items {
		out = append(out, merchantResponseFromService(&items[i]))
	}
	response.Paginated(c, out, result.Total, page, pageSize)
}

// Get 获取单个商户。
// GET /api/v1/admin/merchants/:id
func (h *MerchantHandler) Get(c *gin.Context) {
	id, ok := parseMerchantID(c)
	if !ok {
		return
	}

	m, err := h.merchantService.Get(c.Request.Context(), id)
	if err != nil {
		h.respondError(c, err)
		return
	}
	response.Success(c, merchantResponseFromService(m))
}

// UpdateCommission 调整商户抽成比例。
// PATCH /api/v1/admin/merchants/:id/commission
func (h *MerchantHandler) UpdateCommission(c *gin.Context) {
	id, ok := parseMerchantID(c)
	if !ok {
		return
	}

	var req updateMerchantCommissionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	if err := h.merchantService.SetCommissionRate(c.Request.Context(), id, *req.CommissionRate); err != nil {
		h.respondError(c, err)
		return
	}
	response.Success(c, gin.H{"id": id, "commission_rate": *req.CommissionRate})
}

func (h *MerchantHandler) respondError(c *gin.Context, err error) {
	// 抽成比例是调用方输入问题，映射为 400 而非 500。
	if errors.Is(err, service.ErrCommissionOutOfRange) {
		response.BadRequest(c, err.Error())
		return
	}
	response.ErrorFrom(c, err)
}

func parseMerchantID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid merchant ID")
		return 0, false
	}
	return id, true
}
