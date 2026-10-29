package routes

import (
	"github.com/woolsen/router2api/internal/handler"
	"github.com/woolsen/router2api/internal/server/middleware"
	"github.com/woolsen/router2api/internal/service"

	"github.com/gin-gonic/gin"
)

// RegisterMarketRoutes 注册货架（只读）路由。
//
// 货架用于挑选分组并跳转到密钥创建流程，因此要求登录（与 /keys 一致），
// 并沿用用户面板的 backend 模式守卫与按用户限流。
func RegisterMarketRoutes(
	v1 *gin.RouterGroup,
	h *handler.Handlers,
	jwtAuth middleware.JWTAuthMiddleware,
	settingService *service.SettingService,
	panelRateLimiter *middleware.PanelRateLimiter,
) {
	market := v1.Group("/market")
	market.Use(gin.HandlerFunc(jwtAuth))
	market.Use(middleware.BackendModeUserGuard(settingService))
	market.Use(panelRateLimiter.Global())
	{
		market.GET("/groups", h.Market.List)
	}
}
