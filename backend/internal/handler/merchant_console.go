package handler

import (
	"strconv"

	infraerrors "github.com/woolsen/router2api/internal/pkg/errors"
	"github.com/woolsen/router2api/internal/pkg/response"
	"github.com/woolsen/router2api/internal/server/middleware"
	"github.com/woolsen/router2api/internal/service"

	"github.com/gin-gonic/gin"
)

// MerchantConsoleHandler 提供商户自助后台接口。
// 所有请求的商户作用域由 MerchantScopeMiddleware 注入，处理器不读取任何
// query/body 中的 merchant_id。
type MerchantConsoleHandler struct {
	consoleService service.MerchantConsoleReader
}

func NewMerchantConsoleHandler(consoleService service.MerchantConsoleReader) *MerchantConsoleHandler {
	return &MerchantConsoleHandler{consoleService: consoleService}
}

type merchantDashboardResponse struct {
	MerchantID     int64   `json:"merchant_id"`
	Name           string  `json:"name"`
	CommissionRate float64 `json:"commission_rate"`
	PayableBalance float64 `json:"payable_balance"`
}

type merchantDailyUsagePointResponse struct {
	Date       string  `json:"date"`
	Requests   int64   `json:"requests"`
	GrossCost  float64 `json:"gross_cost"`
	Commission float64 `json:"commission"`
	NetPayable float64 `json:"net_payable"`
}

type merchantUsageTrendResponse struct {
	Days      int                               `json:"days"`
	StartDate string                            `json:"start_date"`
	EndDate   string                            `json:"end_date"`
	Items     []merchantDailyUsagePointResponse `json:"items"`
}

// Me 返回当前登录商户的概览。
// GET /api/v1/merchant/me
func (h *MerchantConsoleHandler) Me(c *gin.Context) {
	merchantID, ok := requireMerchantScope(c)
	if !ok {
		return
	}

	dashboard, err := h.consoleService.Dashboard(c.Request.Context(), merchantID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	response.Success(c, merchantDashboardResponse{
		MerchantID:     dashboard.MerchantID,
		Name:           dashboard.Name,
		CommissionRate: dashboard.CommissionRate,
		PayableBalance: dashboard.PayableBalance,
	})
}

// DailyUsage 返回当前商户最近 N 天（默认 7，最多 30）的计费用量。
// GET /api/v1/merchant/usage/daily?days=7
func (h *MerchantConsoleHandler) DailyUsage(c *gin.Context) {
	merchantID, ok := requireMerchantScope(c)
	if !ok {
		return
	}

	days := service.MerchantUsageTrendDefaultDays
	if raw := c.Query("days"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			response.BadRequest(c, "days must be a positive integer")
			return
		}
		days = parsed
	}

	trend, err := h.consoleService.DailyUsage(c.Request.Context(), merchantID, days)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	items := make([]merchantDailyUsagePointResponse, 0, len(trend.Points))
	for _, point := range trend.Points {
		items = append(items, merchantDailyUsagePointResponse{
			Date:       point.Date,
			Requests:   point.Requests,
			GrossCost:  point.GrossCost,
			Commission: point.Commission,
			NetPayable: point.NetPayable,
		})
	}

	response.Success(c, merchantUsageTrendResponse{
		Days:      trend.Days,
		StartDate: trend.StartDate,
		EndDate:   trend.EndDate,
		Items:     items,
	})
}

// requireMerchantScope 只从中间件上下文取商户 ID。
// 直接调用未经过 MerchantScopeMiddleware 的处理器时按无权访问处理。
func requireMerchantScope(c *gin.Context) (int64, bool) {
	merchantID, ok := middleware.GetMerchantIDFromContext(c)
	if !ok {
		response.ErrorFrom(c, infraerrors.Forbidden("MERCHANT_ONLY", "Merchant access required"))
		return 0, false
	}
	return merchantID, true
}
