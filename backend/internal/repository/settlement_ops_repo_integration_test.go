//go:build integration

package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/woolsen/router2api/internal/service"
)

func TestSettlementOpsRepositoryIntegrationGenerateWritesItemsAndDeadline(t *testing.T) {
	ctx := context.Background()
	merchantID := int64(991002)
	from := time.Now().UTC().Add(-9 * 24 * time.Hour).Truncate(time.Second)
	to := from.Add(48 * time.Hour)

	var userID, accountID, apiKeyID int64
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`INSERT INTO users (email, password_hash) VALUES ($1, 'x') RETURNING id`,
		fmt.Sprintf("settlement-%d@test.invalid", time.Now().UnixNano())).Scan(&userID))
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`INSERT INTO accounts (name, platform, type) VALUES ('settlement-test', 'openai', 'oauth') RETURNING id`).Scan(&accountID))
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`INSERT INTO api_keys (user_id, key, name) VALUES ($1, $2, 'settlement-test') RETURNING id`,
		userID, fmt.Sprintf("sk-settlement-%d", time.Now().UnixNano())).Scan(&apiKeyID))

	insertUsage := func(cost float64, at time.Time) {
		_, err := integrationDB.ExecContext(ctx, `
INSERT INTO usage_logs (user_id, api_key_id, account_id, model, actual_cost, billing_type, merchant_id, commission_rate, created_at)
VALUES ($1, $2, $3, 'settlement-test', $4, $5, $6, 0.1, $7)`,
			userID, apiKeyID, accountID, cost, service.BillingTypeBalance, merchantID, at)
		require.NoError(t, err)
	}
	insertUsage(10, from.Add(time.Hour))
	insertUsage(20, from.Add(2*time.Hour))

	settlementRepo := NewSettlementRepository(integrationEntClient, integrationDB)
	svc := service.NewSettlementService(settlementRepo, nil)
	settlement, err := svc.Generate(ctx, merchantID, from, to)
	require.NoError(t, err)
	require.InDelta(t, 30, settlement.GrossCost, 1e-9)
	require.InDelta(t, 3, settlement.Commission, 1e-9)
	require.InDelta(t, 27, settlement.NetPayable, 1e-9)
	require.NotNil(t, settlement.DisputeDeadline)
	require.WithinDuration(t, time.Now().Add(service.SettlementDisputeWindow), *settlement.DisputeDeadline, time.Minute)

	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM settlement_items WHERE settlement_id = $1`, settlement.ID)
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM settlements WHERE id = $1`, settlement.ID)
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM usage_logs WHERE merchant_id = $1`, merchantID)
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM api_keys WHERE id = $1`, apiKeyID)
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM accounts WHERE id = $1`, accountID)
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, userID)
	})

	ops := service.NewSettlementOps(NewSettlementOpsRepository(integrationEntClient, integrationDB))
	items, err := ops.Explore(ctx, settlement.ID)
	require.NoError(t, err)
	require.Len(t, items, 2)
	var totalCost, totalCommission float64
	for _, item := range items {
		totalCost += item.Cost
		totalCommission += item.Commission
	}
	require.InDelta(t, 30, totalCost, 1e-9)
	require.InDelta(t, 3, totalCommission, 1e-9)

	// 生成时不入账。
	var balanceRows int
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM merchant_balance WHERE merchant_id = $1`, merchantID).Scan(&balanceRows))
	require.Zero(t, balanceRows)
}

func TestSettlementOpsRepositoryIntegrationConfirmCreditsOnce(t *testing.T) {
	ctx := context.Background()
	repo := NewSettlementOpsRepository(integrationEntClient, integrationDB)

	merchantID := int64(991001)
	from := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
	to := from.Add(24 * time.Hour)

	var settlementID int64
	err := integrationDB.QueryRowContext(ctx, `
INSERT INTO settlements (merchant_id, period_start, period_end, gross_cost, commission, net_payable, status)
VALUES ($1, $2, $3, 110, 10, 100, 'pending')
RETURNING id`, merchantID, from, to).Scan(&settlementID)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM merchant_ledger WHERE merchant_id = $1`, merchantID)
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM merchant_balance WHERE merchant_id = $1`, merchantID)
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM settlements WHERE id = $1`, settlementID)
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM audit_logs WHERE path = $1`, fmt.Sprintf("/settlements/%d", settlementID))
	})

	at := time.Now().UTC()
	tr, err := repo.ConfirmSettlement(ctx, settlementID, "integration@test", at)
	require.NoError(t, err)
	require.True(t, tr.Applied)
	require.Equal(t, service.SettlementStatusConfirmed, tr.Status)

	// 重复确认是幂等的，且不会二次加余额。
	tr, err = repo.ConfirmSettlement(ctx, settlementID, "integration@test", at.Add(time.Minute))
	require.NoError(t, err)
	require.False(t, tr.Applied)
	require.Equal(t, service.SettlementStatusConfirmed, tr.Status)

	var balance float64
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT balance FROM merchant_balance WHERE merchant_id = $1`, merchantID).Scan(&balance))
	require.InDelta(t, 100, balance, 1e-9)

	var ledgerRows int
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM merchant_ledger WHERE merchant_id = $1 AND type = 'settle' AND ref_type = 'settlement' AND ref_id = $2`,
		merchantID, settlementID).Scan(&ledgerRows))
	require.Equal(t, 1, ledgerRows)

	// 已确认但未过申诉窗口可以转 disputed；disputed 后不能再确认/入账。
	tr, err = repo.DisputeSettlement(ctx, settlementID, "amount mismatch", at.Add(2*time.Minute))
	require.NoError(t, err)
	require.True(t, tr.Applied)
	require.Equal(t, service.SettlementStatusDisputed, tr.Status)

	tr, err = repo.ConfirmSettlement(ctx, settlementID, "integration@test", at.Add(3*time.Minute))
	require.NoError(t, err)
	require.False(t, tr.Applied)
	require.Equal(t, service.SettlementStatusDisputed, tr.Status)

	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT balance FROM merchant_balance WHERE merchant_id = $1`, merchantID).Scan(&balance))
	require.InDelta(t, 100, balance, 1e-9)
}
