package repository

import (
	"testing"

	"github.com/stretchr/testify/require"
	dbent "github.com/woolsen/router2api/ent"
)

// 作用域中间件依赖 users.merchant_id 被带进 service.User；漏掉这一行会让
// 所有 merchant 账号被误判为未绑定商户。
func TestUserEntityToServiceCarriesMerchantID(t *testing.T) {
	id := int64(17)
	got := userEntityToService(&dbent.User{
		ID:         42,
		Email:      "merchant@example.com",
		Role:       "merchant",
		MerchantID: &id,
	})
	require.NotNil(t, got)
	require.Equal(t, "merchant", got.Role)
	require.NotNil(t, got.MerchantID)
	require.Equal(t, int64(17), *got.MerchantID)

	unbound := userEntityToService(&dbent.User{ID: 43, Role: "merchant"})
	require.NotNil(t, unbound)
	require.Nil(t, unbound.MerchantID)
}
