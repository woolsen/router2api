package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/woolsen/router2api/internal/service"
)

func merchantIDPtr(v int64) *int64 { return &v }

func TestMerchantScopeRejectsNonMerchant(t *testing.T) {
	_, err := MerchantScope(&service.User{Role: service.RoleUser})
	if !errors.Is(err, ErrNotMerchant) {
		t.Fatalf("want ErrNotMerchant, got %v", err)
	}
}

func TestMerchantScopeRejectsMerchantWithoutLink(t *testing.T) {
	for _, user := range []*service.User{
		nil,
		{Role: service.RoleMerchant},
		{Role: service.RoleMerchant, MerchantID: merchantIDPtr(0)},
		{Role: service.RoleMerchant, MerchantID: merchantIDPtr(-1)},
	} {
		if _, err := MerchantScope(user); !errors.Is(err, ErrNotMerchant) {
			t.Fatalf("user %+v: want ErrNotMerchant, got %v", user, err)
		}
	}
}

// 作用域只来自已认证用户本身；客户端提供的 merchant_id 永远不会被读取。
func TestMerchantScopeIgnoresClientSuppliedMerchantID(t *testing.T) {
	id, err := MerchantScope(&service.User{Role: service.RoleMerchant, MerchantID: merchantIDPtr(7)})
	if err != nil || id != 7 {
		t.Fatalf("want 7, got %d %v", id, err)
	}
}

type merchantScopeUserStub struct {
	user *service.User
	err  error
}

func (r *merchantScopeUserStub) GetByID(_ context.Context, _ int64) (*service.User, error) {
	return r.user, r.err
}

func TestMerchantScopeMiddlewareInjectsAuthenticatedMerchant(t *testing.T) {
	gin.SetMode(gin.TestMode)

	reader := &merchantScopeUserStub{
		user: &service.User{ID: 42, Role: service.RoleMerchant, MerchantID: merchantIDPtr(5)},
	}
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(string(ContextKeyUser), AuthSubject{UserID: 42})
		c.Next()
	})
	r.Use(MerchantScopeMiddleware(reader))
	r.GET("/test", func(c *gin.Context) {
		id, ok := GetMerchantIDFromContext(c)
		require.True(t, ok)
		c.JSON(http.StatusOK, gin.H{"merchant_id": id})
	})

	w := httptest.NewRecorder()
	// 攻击者试图在 query 里冒充商户 999，必须被忽略。
	req := httptest.NewRequest(http.MethodGet, "/test?merchant_id=999", nil)
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.JSONEq(t, `{"merchant_id":5}`, w.Body.String())
}

func TestMerchantScopeMiddlewareRejectsNonMerchantWithStableCode(t *testing.T) {
	gin.SetMode(gin.TestMode)

	reader := &merchantScopeUserStub{
		user: &service.User{ID: 42, Role: service.RoleUser},
	}
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(string(ContextKeyUser), AuthSubject{UserID: 42})
		c.Next()
	})
	r.Use(MerchantScopeMiddleware(reader))
	r.GET("/test", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/test", nil))

	require.Equal(t, http.StatusForbidden, w.Code)
	require.Contains(t, w.Body.String(), `"code":"MERCHANT_ONLY"`)
}

func TestMerchantScopeMiddlewareRejectsMissingAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)

	r := gin.New()
	r.Use(MerchantScopeMiddleware(&merchantScopeUserStub{}))
	r.GET("/test", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/test", nil))

	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Contains(t, w.Body.String(), `"code":"UNAUTHORIZED"`)
}
