package handler

import (
	"context"
	"log/slog"
	"sort"
	"time"

	"github.com/woolsen/router2api/internal/pkg/response"
	"github.com/woolsen/router2api/internal/server/middleware"
	"github.com/woolsen/router2api/internal/service"

	"github.com/gin-gonic/gin"
)

// MarketGroupSource 是货架的数据来源：以分组为顶层、附带模型列表的只读查询。
//
// 生产实现是 *service.ModelPlazaService，也就是 /model-plaza 的数据源：模型枚举
// 来自渠道的 SupportedModels（Group 本身没有 models 字段），因此货架与广场看到的
// 模型口径天然一致，不存在第二份模型清单。
type MarketGroupSource interface {
	ListGroups(ctx context.Context) ([]service.PlazaGroup, error)
}

// MarketVisibilitySource 提供当前用户的可见分组集合、公开分组限制开关与专属倍率。
//
// 生产实现是 *service.APIKeyService（与 /model-plaza 完全同源）。货架必须复用这套
// 可见性规则，否则专属分组的名称与倍率会暴露给无权用户，用户也会看到自己绑不了的
// 分组。倍率也必须走同一份用户专属配置：货架展示的必须是用户实际会被扣费的倍率。
type MarketVisibilitySource interface {
	GetUserGroupVisibility(ctx context.Context, userID int64) (map[int64]struct{}, bool, error)
	GetUserGroupRates(ctx context.Context, userID int64) (map[int64]float64, error)
}

// MarketGroupsHandler 处理货架（market）的只读查询。
//
// 货架只展示入驻商户（merchant_id IS NOT NULL）且状态为 active、且对当前用户可见的
// 分组：数据源的 ListGroups 只列 active 分组，这里再按商户归属与可见性过滤，平台
// 自营分组与用户无权访问的专属分组都不上架。
type MarketGroupsHandler struct {
	source     MarketGroupSource
	visibility MarketVisibilitySource
}

// NewMarketGroupsHandler 创建货架 handler。
func NewMarketGroupsHandler(source MarketGroupSource, visibility MarketVisibilitySource) *MarketGroupsHandler {
	return &MarketGroupsHandler{source: source, visibility: visibility}
}

// marketGroup 是货架分组条目（对外白名单字段）。
//
// 明确不包含 merchant_id、商户名称、上游渠道/地址：货架是给使用者挑选分组的，
// 商户身份与上游拓扑属于平台内部信息。
type marketGroup struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Platform string `json:"platform"`
	// RateMultiplier 是当前用户实际生效的 token 倍率：用户专属倍率优先于分组基础倍率
	// （与网关 ResolveUserGroupRateMultiplier 的覆盖语义一致，不是相乘）。峰时倍率
	// 不并入该值，而是通过 PeakRate 单独标注。
	RateMultiplier float64 `json:"rate_multiplier"`
	// RateIsEstimate 为 true 表示服务端拿不到用户专属倍率，RateMultiplier 只是分组
	// 基础倍率；前端必须提示“以实际计费为准”，不能假装精确。
	RateIsEstimate bool            `json:"rate_is_estimate"`
	PeakRate       *marketPeakRate `json:"peak_rate"`
	Models         []string        `json:"models"`
	Status         string          `json:"status"`
	// Stats 在 M1 恒为 null。指标依赖 M2 的 request_metrics；usage_logs 只记成功
	// 请求，拼出来的成功率会误导用户，所以宁可留空让前端显示「数据不足」。
	Stats any `json:"stats"`
}

// marketPeakRate 是分组的高峰时段倍率信息，供前端单独标注。
// 峰值倍率只在订阅分组且当前落在高峰窗口内时才会实际乘进 token 计费。
type marketPeakRate struct {
	Start      string  `json:"start"`
	End        string  `json:"end"`
	Multiplier float64 `json:"multiplier"`
	ActiveNow  bool    `json:"active_now"`
}

// List 返回货架分组列表，按倍率升序（同倍率按名称）。
// GET /api/v1/market/groups
func (h *MarketGroupsHandler) List(c *gin.Context) {
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok {
		response.Unauthorized(c, "User not authenticated")
		return
	}

	// 与 /model-plaza 相同的可见性裁剪：未授权的专属分组、受限用户的公开分组
	// 都不能出现在货架上。
	allowedGroups, restrictPublicGroups, err := h.visibility.GetUserGroupVisibility(c.Request.Context(), subject.UserID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	// 用户专属倍率是“实际生效倍率”的组成部分。查不到时不能静默按基础倍率假装精确：
	// 标记 rate_is_estimate 让前端提示以实际计费为准。
	userRates, ratesErr := h.visibility.GetUserGroupRates(c.Request.Context(), subject.UserID)
	if ratesErr != nil {
		slog.Warn("market_user_rates_failed", "error", ratesErr, "user_id", subject.UserID)
		userRates = nil
	}

	groups, err := h.source.ListGroups(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	visible := make([]service.PlazaGroup, 0, len(groups))
	for i := range groups {
		// 平台自营分组（merchant_id IS NULL）不是货架商品。
		if groups[i].MerchantID == nil {
			continue
		}
		visible = append(visible, groups[i])
	}
	visible = filterPlazaVisibleGroups(visible, allowedGroups, restrictPublicGroups)

	acceptLanguage := c.GetHeader("Accept-Language")
	now := time.Now()
	out := make([]marketGroup, 0, len(visible))
	for i := range visible {
		g := &visible[i]
		models := make([]string, 0, len(g.Models))
		for j := range g.Models {
			models = append(models, g.Models[j].Name)
		}
		// 用户专属倍率覆盖分组基础倍率（与网关一致）；缺省回落到基础倍率。
		rate := g.RateMultiplier
		if userRate, ok := userRates[g.ID]; ok {
			rate = userRate
		}
		// 图片/视频独立倍率不受分组 token 倍率约束，单值展示只能算估算。
		estimate := ratesErr != nil || g.ImageRateIndependent || g.VideoRateIndependent
		out = append(out, marketGroup{
			ID:             g.ID,
			Name:           g.I18n.LocalizeName(acceptLanguage, g.Name),
			Platform:       g.Platform,
			RateMultiplier: rate,
			RateIsEstimate: estimate,
			PeakRate:       marketPeakRateFrom(g, now),
			Models:         models,
			// ListGroups 只返回 active 分组；状态字段随契约输出，客户端不必推断。
			Status: service.StatusActive,
			Stats:  nil,
		})
	}

	// 倍率升序是货架的排序契约：不设「推荐」位，也不引入排序分。
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].RateMultiplier != out[j].RateMultiplier {
			return out[i].RateMultiplier < out[j].RateMultiplier
		}
		return out[i].Name < out[j].Name
	})

	// 分组名称随请求语言变化，缓存键必须带上语言。
	c.Writer.Header().Add("Vary", "Accept-Language")
	response.Success(c, out)
}

// marketPeakRateFrom 返回高峰倍率信息；未启用或配置无效时返回 nil。
// active_now 由与计费完全相同的 Group.PeakMultiplierAt 判定，避免展示与实扣口径漂移。
func marketPeakRateFrom(g *service.PlazaGroup, now time.Time) *marketPeakRate {
	if g == nil || !g.PeakRateEnabled || g.PeakStart == "" || g.PeakEnd == "" {
		return nil
	}
	group := &service.Group{
		SubscriptionType:   g.SubscriptionType,
		PeakRateEnabled:    g.PeakRateEnabled,
		PeakStart:          g.PeakStart,
		PeakEnd:            g.PeakEnd,
		PeakRateMultiplier: g.PeakRateMultiplier,
	}
	return &marketPeakRate{
		Start:      g.PeakStart,
		End:        g.PeakEnd,
		Multiplier: g.PeakRateMultiplier,
		ActiveNow:  group.IsPeakTimeAt(now),
	}
}
