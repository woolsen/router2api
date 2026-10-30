package handler

import (
	"errors"

	"github.com/woolsen/router2api/internal/pkg/response"
	"github.com/woolsen/router2api/internal/server/middleware"
	"github.com/woolsen/router2api/internal/service"

	"github.com/gin-gonic/gin"
)

// MerchantApplyHandler 提供商户自助入驻：先探测上游，再提交申请自动上架。
// 两个接口都只使用已认证用户 ID，不允许客户端指定 merchant_id。
type MerchantApplyHandler struct {
	probe *service.ProbeService
	apply *service.MerchantApplyService
}

func NewMerchantApplyHandler(probe *service.ProbeService, apply *service.MerchantApplyService) *MerchantApplyHandler {
	return &MerchantApplyHandler{probe: probe, apply: apply}
}

type merchantProbeRequest struct {
	BaseURL  string `json:"base_url" binding:"required"`
	APIKey   string `json:"api_key" binding:"required"`
	Platform string `json:"platform" binding:"required"`
}

type merchantApplyRequest struct {
	MerchantName   string   `json:"merchant_name" binding:"required"`
	BaseURL        string   `json:"base_url" binding:"required"`
	APIKey         string   `json:"api_key" binding:"required"`
	Platform       string   `json:"platform" binding:"required"`
	RateMultiplier float64  `json:"rate_multiplier"`
	Models         []string `json:"models"`
}

type merchantProbeResponse struct {
	Models         []string `json:"models"`
	SupportsStream bool     `json:"supports_stream"`
	ReturnsUsage   bool     `json:"returns_usage"`
	LatencyMS      int      `json:"latency_ms"`
}

type merchantApplyResponse struct {
	ID             int64   `json:"id"`
	Name           string  `json:"name"`
	Status         string  `json:"status"`
	CommissionRate float64 `json:"commission_rate"`
}

// Probe 探测商户上游的四项能力。
// POST /api/v1/merchant/apply/probe
func (h *MerchantApplyHandler) Probe(c *gin.Context) {
	if _, ok := middleware.GetAuthSubjectFromContext(c); !ok {
		response.Unauthorized(c, "User not authenticated")
		return
	}
	var req merchantProbeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	result, err := h.probe.Probe(c.Request.Context(), req.BaseURL, req.APIKey, req.Platform)
	if err != nil {
		respondMerchantApplyError(c, err)
		return
	}
	response.Success(c, merchantProbeResponse{
		Models:         result.Models,
		SupportsStream: result.SupportsStream,
		ReturnsUsage:   result.ReturnsUsage,
		LatencyMS:      result.LatencyMS,
	})
}

// Submit 提交申请：探测通过后自动建商户、建分组、接入账号并上架。
// POST /api/v1/merchant/apply
func (h *MerchantApplyHandler) Submit(c *gin.Context) {
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok {
		response.Unauthorized(c, "User not authenticated")
		return
	}
	var req merchantApplyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	merchant, err := h.apply.Submit(c.Request.Context(), subject.UserID, service.ApplyInput{
		MerchantName:   req.MerchantName,
		BaseURL:        req.BaseURL,
		APIKey:         req.APIKey,
		Platform:       req.Platform,
		RateMultiplier: req.RateMultiplier,
		Models:         req.Models,
	})
	if err != nil {
		respondMerchantApplyError(c, err)
		return
	}
	response.Created(c, merchantApplyResponse{
		ID:             merchant.ID,
		Name:           merchant.Name,
		Status:         merchant.Status,
		CommissionRate: merchant.CommissionRate,
	})
}

// respondMerchantApplyError 把固定的探测错误码原样返回给申请者；
// 其余错误沿用统一的应用错误映射。
func respondMerchantApplyError(c *gin.Context, err error) {
	for _, probeErr := range []error{
		service.ErrProbeInsecureURL,
		service.ErrProbeModelsEmpty,
		service.ErrProbeReturnsNoUsage,
		service.ErrProbeStreamUnsupported,
	} {
		if errors.Is(err, probeErr) {
			response.BadRequest(c, probeErr.Error())
			return
		}
	}
	response.ErrorFrom(c, err)
}
