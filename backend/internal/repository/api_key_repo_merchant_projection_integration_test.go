//go:build integration

package repository

// 投影漏列回归（repository 半程）：真实 PostgreSQL 上认证专用查询
// GetByKeyForAuth 的分组显式投影必须携带 groups.merchant_id。网关 handler 把
// 这条查询物化出的 apiKey.Group 传给 RecordUsage，漏列会让 usage_logs.merchant_id
// 恒为 NULL——测试全绿但生产上商户分账整条链路为空。
// 新增快照分组字段时必须同步扩展 WithGroup Select 并在本测试补断言。

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/woolsen/router2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestGetByKeyForAuthCarriesMerchantProjection(t *testing.T) {
	ctx := context.Background()
	suffix := time.Now().UnixNano()
	merchantID := int64(90210)
	group := mustCreateGroup(t, integrationEntClient, &service.Group{
		Name:           fmt.Sprintf("merchant-proj-group-%d", suffix),
		Platform:       service.PlatformOpenAI,
		RateMultiplier: 0.06,
		MerchantID:     &merchantID,
	})
	user := mustCreateUser(t, integrationEntClient, &service.User{
		Email: fmt.Sprintf("merchant-proj-%d@example.com", suffix), Concurrency: 5,
	})
	groupID := group.ID
	keyValue := fmt.Sprintf("sk-merchant-proj-%d", suffix)
	apiKeyRepo := NewAPIKeyRepository(integrationEntClient, integrationDB)
	key := &service.APIKey{UserID: user.ID, GroupID: &groupID, Key: keyValue, Name: "merchant-proj", Status: service.StatusActive}
	require.NoError(t, apiKeyRepo.Create(ctx, key))
	t.Cleanup(func() {
		_, err := integrationDB.ExecContext(ctx, "DELETE FROM auth_cache_invalidation_outbox WHERE cache_key = encode(sha256(convert_to($1, 'UTF8')), 'hex')", keyValue)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(ctx, "DELETE FROM api_keys WHERE id = $1", key.ID)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(ctx, "DELETE FROM users WHERE id = $1", user.ID)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(ctx, "DELETE FROM groups WHERE id = $1", group.ID)
		require.NoError(t, err)
	})

	got, err := apiKeyRepo.GetByKeyForAuth(ctx, keyValue)
	require.NoError(t, err)
	require.NotNil(t, got.Group, "认证查询必须带出分组")
	require.NotNil(t, got.Group.MerchantID, "merchant_id 必须进入认证投影（否则用量行永远没有商户归属）")
	require.Equal(t, merchantID, *got.Group.MerchantID)
}

func TestGetByKeyForAuthKeepsSelfOperatedMerchantNil(t *testing.T) {
	ctx := context.Background()
	suffix := time.Now().UnixNano()
	group := mustCreateGroup(t, integrationEntClient, &service.Group{
		Name:           fmt.Sprintf("self-operated-proj-group-%d", suffix),
		Platform:       service.PlatformOpenAI,
		RateMultiplier: 0.06,
	})
	user := mustCreateUser(t, integrationEntClient, &service.User{
		Email: fmt.Sprintf("self-operated-proj-%d@example.com", suffix), Concurrency: 5,
	})
	groupID := group.ID
	keyValue := fmt.Sprintf("sk-self-operated-proj-%d", suffix)
	apiKeyRepo := NewAPIKeyRepository(integrationEntClient, integrationDB)
	key := &service.APIKey{UserID: user.ID, GroupID: &groupID, Key: keyValue, Name: "self-operated-proj", Status: service.StatusActive}
	require.NoError(t, apiKeyRepo.Create(ctx, key))
	t.Cleanup(func() {
		_, err := integrationDB.ExecContext(ctx, "DELETE FROM auth_cache_invalidation_outbox WHERE cache_key = encode(sha256(convert_to($1, 'UTF8')), 'hex')", keyValue)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(ctx, "DELETE FROM api_keys WHERE id = $1", key.ID)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(ctx, "DELETE FROM users WHERE id = $1", user.ID)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(ctx, "DELETE FROM groups WHERE id = $1", group.ID)
		require.NoError(t, err)
	})

	got, err := apiKeyRepo.GetByKeyForAuth(ctx, keyValue)
	require.NoError(t, err)
	require.NotNil(t, got.Group)
	require.Nil(t, got.Group.MerchantID, "自营分组认证投影必须保持 merchant_id 为 NULL")
}
