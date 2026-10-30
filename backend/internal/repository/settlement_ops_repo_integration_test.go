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

	// 封账后迟到的 usage 行：重跑 Generate 返回同一张单，且不得追加明细。
	// 否则 SUM(settlement_items.cost) 会超过已封账的 gross_cost。
	insertUsage(5, from.Add(3*time.Hour))
	again, err := svc.Generate(ctx, merchantID, from, to)
	require.NoError(t, err)
	require.Equal(t, settlement.ID, again.ID)
	require.InDelta(t, 30, again.GrossCost, 1e-9)

	items, err = ops.Explore(ctx, settlement.ID)
	require.NoError(t, err)
	require.Len(t, items, 2, "重跑不得追加迟到 usage 的明细")
	totalCost, totalCommission = 0, 0
	for _, item := range items {
		totalCost += item.Cost
		totalCommission += item.Commission
	}
	require.InDelta(t, 30, totalCost, 1e-9)
	require.InDelta(t, 3, totalCommission, 1e-9)
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

	// 已确认但未过申诉窗口可以转 disputed；已入账净额必须从可用余额转入冻结。
	tr, err = repo.DisputeSettlement(ctx, settlementID, "amount mismatch", at.Add(2*time.Minute))
	require.NoError(t, err)
	require.True(t, tr.Applied)
	require.Equal(t, service.SettlementStatusDisputed, tr.Status)

	var frozen float64
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT balance, frozen FROM merchant_balance WHERE merchant_id = $1`, merchantID).
		Scan(&balance, &frozen))
	require.InDelta(t, 0, balance, 1e-9, "申诉冻结后可用余额必须扣减")
	require.InDelta(t, 100, frozen, 1e-9, "冻结额必须等于结算净额")

	var holdRows int
	var holdAmount float64
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(amount), 0) FROM merchant_ledger WHERE merchant_id = $1 AND type = 'dispute_hold' AND ref_type = 'settlement' AND ref_id = $2`,
		merchantID, settlementID).Scan(&holdRows, &holdAmount))
	require.Equal(t, 1, holdRows)
	require.InDelta(t, -100, holdAmount, 1e-9, "dispute_hold 记 -net_payable")

	// 重复申诉不重复冻结。
	tr, err = repo.DisputeSettlement(ctx, settlementID, "amount mismatch", at.Add(150*time.Second))
	require.NoError(t, err)
	require.False(t, tr.Applied)
	require.Equal(t, service.SettlementStatusDisputed, tr.Status)
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT balance, frozen FROM merchant_balance WHERE merchant_id = $1`, merchantID).
		Scan(&balance, &frozen))
	require.InDelta(t, 0, balance, 1e-9)
	require.InDelta(t, 100, frozen, 1e-9)

	tr, err = repo.ConfirmSettlement(ctx, settlementID, "integration@test", at.Add(3*time.Minute))
	require.NoError(t, err)
	require.False(t, tr.Applied)
	require.Equal(t, service.SettlementStatusDisputed, tr.Status)
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT balance FROM merchant_balance WHERE merchant_id = $1`, merchantID).Scan(&balance))
	require.InDelta(t, 0, balance, 1e-9)
}

func TestSettlementOpsRepositoryIntegrationDisputeConfirmedAllowsNegativeBalance(t *testing.T) {
	ctx := context.Background()
	repo := NewSettlementOpsRepository(integrationEntClient, integrationDB)

	merchantID := int64(991004)
	from := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
	to := from.Add(24 * time.Hour)

	var settlementID int64
	err := integrationDB.QueryRowContext(ctx, `
INSERT INTO settlements (merchant_id, period_start, period_end, gross_cost, commission, net_payable, status)
VALUES ($1, $2, $3, 110, 10, 100, 'confirmed')
RETURNING id`, merchantID, from, to).Scan(&settlementID)
	require.NoError(t, err)
	// 模拟商户已提现一部分：确认入账 100 后只剩 40 可用。
	_, err = integrationDB.ExecContext(ctx, `
INSERT INTO merchant_balance (merchant_id, balance, frozen) VALUES ($1, 40, 0)
ON CONFLICT (merchant_id) DO UPDATE SET balance = 40, frozen = 0`, merchantID)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM merchant_ledger WHERE merchant_id = $1`, merchantID)
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM merchant_balance WHERE merchant_id = $1`, merchantID)
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM settlements WHERE id = $1`, settlementID)
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM audit_logs WHERE path = $1`, fmt.Sprintf("/settlements/%d", settlementID))
	})

	tr, err := repo.DisputeSettlement(ctx, settlementID, "amount mismatch", time.Now().UTC())
	require.NoError(t, err)
	require.True(t, tr.Applied)

	var balance, frozen float64
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT balance, frozen FROM merchant_balance WHERE merchant_id = $1`, merchantID).
		Scan(&balance, &frozen))
	require.InDelta(t, -60, balance, 1e-9, "余额不足也必须冻结，允许 balance 为负")
	require.InDelta(t, 100, frozen, 1e-9)
}

func TestSettlementOpsRepositoryIntegrationDisputePendingDoesNotFreeze(t *testing.T) {
	ctx := context.Background()
	repo := NewSettlementOpsRepository(integrationEntClient, integrationDB)

	merchantID := int64(991003)
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

	tr, err := repo.DisputeSettlement(ctx, settlementID, "amount mismatch", time.Now().UTC())
	require.NoError(t, err)
	require.True(t, tr.Applied)
	require.Equal(t, service.SettlementStatusDisputed, tr.Status)

	var balanceRows, ledgerRows int
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM merchant_balance WHERE merchant_id = $1`, merchantID).Scan(&balanceRows))
	require.Zero(t, balanceRows, "pending 从未入账，申诉不得冻结或创建余额行")
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM merchant_ledger WHERE merchant_id = $1`, merchantID).Scan(&ledgerRows))
	require.Zero(t, ledgerRows)
}
