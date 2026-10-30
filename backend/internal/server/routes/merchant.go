package routes

import (
	"github.com/woolsen/router2api/internal/handler"
	"github.com/woolsen/router2api/internal/server/middleware"
	"github.com/woolsen/router2api/internal/service"

	"github.com/gin-gonic/gin"
)

// RegisterMerchantRoutes 注册商户自助后台路由（需要认证 + 商户作用域）。
//
// merchant_id 只由 MerchantScopeMiddleware 按已认证用户注入，
// 处理器与查询参数都不能覆盖它。
func RegisterMerchantRoutes(
	v1 *gin.RouterGroup,
	h *handler.Handlers,
	jwtAuth middleware.JWTAuthMiddleware,
	userService *service.UserService,
	settingService *service.SettingService,
	panelRateLimiter *middleware.PanelRateLimiter,
) {
	// 自助入驻面向普通用户，不能挂在 MerchantScope 之后：申请者此时还没有
	// merchant_id。探测与提交都只按已认证用户 ID 记账。
	apply := v1.Group("/merchant/apply")
	apply.Use(gin.HandlerFunc(jwtAuth))
	apply.Use(middleware.BackendModeUserGuard(settingService))
	apply.Use(panelRateLimiter.Global())
	{
		apply.POST("/probe", panelRateLimiter.Heavy(), h.MerchantApply.Probe)
		apply.POST("", panelRateLimiter.Heavy(), h.MerchantApply.Submit)
	}

	merchant := v1.Group("/merchant")
	merchant.Use(gin.HandlerFunc(jwtAuth))
	merchant.Use(middleware.BackendModeUserGuard(settingService))
	merchant.Use(middleware.MerchantScopeMiddleware(userService))
	merchant.Use(panelRateLimiter.Global())
	{
		merchant.GET("/me", h.MerchantConsole.Me)
		merchant.GET("/usage/daily", panelRateLimiter.Heavy(), h.MerchantConsole.DailyUsage)
	}
}
