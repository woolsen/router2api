package service

// 认证快照必须搬运分组的 merchant_id：网关 handler 传给 RecordUsage 的正是
// 认证快照物化出来的 apiKey.Group，漏字段会让 usage_logs.merchant_id 恒为 NULL，
// 商户分账在真实流量上变成空操作。这是 build → L2 JSON 往返 → 还原的回归。

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func merchantAuthSnapshotAPIKey(merchantID *int64) *APIKey {
	groupID := int64(51)
	return &APIKey{
		ID:      83,
		UserID:  41,
		GroupID: &groupID,
		Key:     "sk-merchant-auth-roundtrip",
		Name:    "merchant-auth-roundtrip",
		Status:  StatusActive,
		User: &User{
			ID:          41,
			Email:       "merchant-auth@test.local",
			Status:      StatusActive,
			Concurrency: 5,
		},
		Group: &Group{
			ID:             groupID,
			Name:           "merchant-roundtrip",
			Platform:       PlatformOpenAI,
			Status:         StatusActive,
			Hydrated:       true,
			RateMultiplier: 1,
			MerchantID:     merchantID,
		},
	}
}

func TestAPIKeyAuthSnapshotCarriesMerchantID(t *testing.T) {
	svc := &APIKeyService{}
	merchantID := int64(3)
	apiKey := merchantAuthSnapshotAPIKey(&merchantID)

	snapshot := svc.snapshotFromAPIKey(context.Background(), apiKey)
	require.NotNil(t, snapshot)
	require.Equal(t, apiKeyAuthSnapshotVersion, snapshot.Version)
	require.NotNil(t, snapshot.Group)
	require.NotNil(t, snapshot.Group.MerchantID, "认证快照必须携带 merchant_id")
	require.Equal(t, merchantID, *snapshot.Group.MerchantID)

	// 与 apiKeyCache.SetAuthCache/GetAuthCache 同构的完整 JSON 往返。
	payload, err := json.Marshal(&APIKeyAuthCacheEntry{Snapshot: snapshot})
	require.NoError(t, err)
	var restored APIKeyAuthCacheEntry
	require.NoError(t, json.Unmarshal(payload, &restored))

	materialized, used, err := svc.applyAuthCacheEntry(apiKey.Key, &restored)
	require.NoError(t, err)
	require.True(t, used)
	require.NotNil(t, materialized.Group)
	require.NotNil(t, materialized.Group.MerchantID,
		"还原后的认证分组必须带 merchant_id，否则用量快照直接早退")
	require.Equal(t, merchantID, *materialized.Group.MerchantID)
}

// 端到端闭环：认证快照 → 物化 apiKey → RecordUsage → usage 行带商户快照。
// 这是 Critical 的直接回归：只用手工构造的 Group 会绕过认证投影。
func TestUsageLogSnapshotsMerchantFromAuthCacheMaterializedGroup(t *testing.T) {
	authSvc := &APIKeyService{}
	merchantID := int64(3)
	authKey := merchantAuthSnapshotAPIKey(&merchantID)
	payload, err := json.Marshal(&APIKeyAuthCacheEntry{Snapshot: authSvc.snapshotFromAPIKey(context.Background(), authKey)})
	require.NoError(t, err)
	var restored APIKeyAuthCacheEntry
	require.NoError(t, json.Unmarshal(payload, &restored))
	materialized, used, err := authSvc.applyAuthCacheEntry(authKey.Key, &restored)
	require.NoError(t, err)
	require.True(t, used)

	usageRepo := &merchantSnapshotUsageLogRepo{}
	merchantRepo := &merchantSnapshotMerchantRepo{
		merchants: map[int64]*Merchant{
			3: {ID: 3, Name: "上游三号", Status: MerchantStatusActive, CommissionRate: 0.11},
		},
	}
	gw := newMerchantSnapshotGatewayServiceForTest(usageRepo, merchantRepo)
	err = gw.RecordUsage(context.Background(), &RecordUsageInput{
		Result: &ForwardResult{
			RequestID: "merchant-snapshot-via-auth-cache",
			Model:     "claude-sonnet-4",
			Usage:     ClaudeUsage{InputTokens: 20, OutputTokens: 5},
			Duration:  time.Second,
		},
		APIKey:  materialized,
		User:    materialized.User,
		Account: &Account{ID: 31},
	})
	require.NoError(t, err)
	require.NotNil(t, usageRepo.lastLog)
	require.NotNil(t, usageRepo.lastLog.MerchantID, "认证路径的用量行必须带 merchant_id（投影/快照漏字段时这里最先失败）")
	require.Equal(t, merchantID, *usageRepo.lastLog.MerchantID)
	require.NotNil(t, usageRepo.lastLog.CommissionRate)
	require.InDelta(t, 0.11, *usageRepo.lastLog.CommissionRate, 1e-9)
}

func TestAPIKeyAuthSnapshotKeepsSelfOperatedGroupMerchantNil(t *testing.T) {
	svc := &APIKeyService{}
	apiKey := merchantAuthSnapshotAPIKey(nil)

	snapshot := svc.snapshotFromAPIKey(context.Background(), apiKey)
	require.NotNil(t, snapshot.Group)
	require.Nil(t, snapshot.Group.MerchantID)

	payload, err := json.Marshal(&APIKeyAuthCacheEntry{Snapshot: snapshot})
	require.NoError(t, err)
	var restored APIKeyAuthCacheEntry
	require.NoError(t, json.Unmarshal(payload, &restored))

	materialized, used, err := svc.applyAuthCacheEntry(apiKey.Key, &restored)
	require.NoError(t, err)
	require.True(t, used)
	require.NotNil(t, materialized.Group)
	require.Nil(t, materialized.Group.MerchantID, "自营分组不得被写成 0 或凭空生出商户")
}
