package admin

import (
	"errors"
	"strconv"

	"github.com/woolsen/router2api/internal/pkg/pagination"
	"github.com/woolsen/router2api/internal/pkg/response"
	"github.com/woolsen/router2api/internal/service"

	"github.com/gin-gonic/gin"
)

// MerchantHandler 管理入驻商户（中转站）的后台 CRUD 与分组接入。
type MerchantHandler struct {
	merchantService   *service.MerchantService
	onboardingService *service.MerchantOnboardingService
}

func NewMerchantHandler(
	merchantService *service.MerchantService,
	onboardingService *service.MerchantOnboardingService,
) *MerchantHandler {
	return &MerchantHandler{
		merchantService:   merchantService,
		onboardingService: onboardingService,
	}
}

type createMerchantRequest struct {
	Name           string   `json:"name" binding:"required"`
	Contact        string   `json:"contact"`
	CommissionRate *float64 `json:"commission_rate"`
}

type updateMerchantCommissionRequest struct {
	CommissionRate *float64 `json:"commission_rate" binding:"required"`
}

// attachMerchantGroupRequest 是商户把分组接入上游的请求体。
// models 不能为空：货架只展示有模型的分组，无模型接入等于不可售。
type attachMerchantGroupRequest struct {
	GroupID  int64    `json:"group_id" binding:"required"`
	BaseURL  string   `json:"base_url" binding:"required"`
	APIKey   string   `json:"api_key" binding:"required"`
	Platform string   `json:"platform" binding:"required"`
	Models   []string `json:"models"`
}

// attachMerchantGroupResponse 是接入成功的白名单响应：绝不回显 api_key/base_url。
type attachMerchantGroupResponse struct {
	GroupID       int64    `json:"group_id"`
	MerchantID    int64    `json:"merchant_id"`
	AccountID     int64    `json:"account_id"`
	AccountName   string   `json:"account_name"`
	AccountStatus string   `json:"account_status"`
	Platform      string   `json:"platform"`
	Models        []string `json:"models"`
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

// AttachGroup 为商户接入一个分组的商户上游账号，并配置货架所需模型。
// POST /api/v1/admin/merchants/:id/groups
func (h *MerchantHandler) AttachGroup(c *gin.Context) {
	id, ok := parseMerchantID(c)
	if !ok {
		return
	}

	var req attachMerchantGroupRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	if req.GroupID <= 0 {
		response.BadRequest(c, "Invalid group ID")
		return
	}
	if len(req.Models) == 0 {
		response.ErrorFrom(c, service.ErrMerchantGroupModelsRequired)
		return
	}

	account, err := h.onboardingService.AttachGroup(c.Request.Context(), service.AttachGroupInput{
		MerchantID: id,
		GroupID:    req.GroupID,
		BaseURL:    req.BaseURL,
		APIKey:     req.APIKey,
		Platform:   req.Platform,
	})
	if err != nil {
		h.respondError(c, err)
		return
	}
	if err := h.onboardingService.ConfigureGroupModels(c.Request.Context(), req.GroupID, req.Platform, req.Models); err != nil {
		h.respondError(c, err)
		return
	}

	response.Success(c, attachMerchantGroupResponse{
		GroupID:       req.GroupID,
		MerchantID:    id,
		AccountID:     account.ID,
		AccountName:   account.Name,
		AccountStatus: account.Status,
		Platform:      account.Platform,
		Models:        req.Models,
	})
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
