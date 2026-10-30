package middleware

import (
	"context"
	"errors"

	"github.com/woolsen/router2api/internal/service"

	"github.com/gin-gonic/gin"
)

// ContextKeyMerchantID 是商户请求作用域上下文键。
// 仅由 MerchantScopeMiddleware 在认证后写入，处理器不得从 query/body 读取。
const ContextKeyMerchantID ContextKey = "merchant_id"

// CodeMerchantOnly 是非商户访问商户接口时的稳定错误码。
const CodeMerchantOnly = "MERCHANT_ONLY"

// ErrNotMerchant 表示当前用户不具备商户身份或未绑定商户。
var ErrNotMerchant = errors.New("user is not a merchant")

// MerchantScope 从已认证用户解析商户作用域。
//
// 角色不是 merchant 或 merchant_id 为空/非正数时返回 ErrNotMerchant。
// 入参只来自认证中间件加载的用户，绝不接受请求参数。
func MerchantScope(user *service.User) (int64, error) {
	if user == nil || user.Role != service.RoleMerchant {
		return 0, ErrNotMerchant
	}
	if user.MerchantID == nil || *user.MerchantID <= 0 {
		return 0, ErrNotMerchant
	}
	return *user.MerchantID, nil
}

type merchantScopeUserReader interface {
	GetByID(ctx context.Context, id int64) (*service.User, error)
}

// MerchantScopeMiddleware 在 JWT 认证之后加载用户并注入商户作用域。
//
// AuthSubject 只携带 UserID/Concurrency，JWT 里没有 merchant_id，
// 因此必须按 UserID 回查 users.merchant_id，而不是信任任何客户端输入。
func MerchantScopeMiddleware(users merchantScopeUserReader) gin.HandlerFunc {
	return func(c *gin.Context) {
		subject, ok := GetAuthSubjectFromContext(c)
		if !ok || subject.UserID <= 0 {
			AbortWithError(c, 401, "UNAUTHORIZED", "User not authenticated")
			return
		}

		user, err := users.GetByID(c.Request.Context(), subject.UserID)
		if err != nil {
			if errors.Is(err, service.ErrUserNotFound) {
				AbortWithError(c, 401, "USER_NOT_FOUND", "User not found")
				return
			}
			AbortWithError(c, 500, "INTERNAL_ERROR", "Failed to load user")
			return
		}

		merchantID, err := MerchantScope(user)
		if err != nil {
			AbortWithError(c, 403, CodeMerchantOnly, "Merchant access required")
			return
		}

		c.Set(string(ContextKeyMerchantID), merchantID)
		c.Next()
	}
}

// GetMerchantIDFromContext 返回 MerchantScopeMiddleware 注入的商户 ID。
// 未经过该中间件的请求必须视为无权访问。
func GetMerchantIDFromContext(c *gin.Context) (int64, bool) {
	value, exists := c.Get(string(ContextKeyMerchantID))
	if !exists {
		return 0, false
	}
	id, ok := value.(int64)
	if !ok || id <= 0 {
		return 0, false
	}
	return id, true
}
