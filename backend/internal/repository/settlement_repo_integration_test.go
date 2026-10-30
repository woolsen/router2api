//go:build integration

package repository

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/woolsen/router2api/internal/service"
	"github.com/woolsen/router2api/migrations"
)

// 两个独立连接同时 CreateOrGet 同一账期：必须拿到同一行，且只落一行。
//
// 先在 conn1 上持有一个未提交的 INSERT，让两个 CreateOrGet 都堵在唯一索引冲突上；
// conn1 提交后，它们各自执行下一条 SELECT。旧实现把 INSERT 和 SELECT 放在同一条
// CTE 里，SELECT 仍用语句开始时的快照，会看不到刚提交的行并返回 sql.ErrNoRows。
func TestSettlementRepositoryIntegrationCreateOrGetConcurrentReturnsSingleRow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	merchantID := time.Now().UnixNano()
	from := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
	to := from.Add(24 * time.Hour)

	conn1, err := integrationDB.Conn(ctx)
	require.NoError(t, err)
	conn2, err := integrationDB.Conn(ctx)
	require.NoError(t, err)
	conn3, err := integrationDB.Conn(ctx)
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(),
			`DELETE FROM settlement_items WHERE settlement_id IN (SELECT id FROM settlements WHERE merchant_id = $1)`, merchantID)
		_, _ = integrationDB.ExecContext(context.Background(),
			`DELETE FROM settlements WHERE merchant_id = $1`, merchantID)
	})
	t.Cleanup(func() {
		_ = conn1.Close()
		_ = conn2.Close()
		_ = conn3.Close()
	})

	tx, err := conn1.BeginTx(ctx, nil)
	require.NoError(t, err)

	var existingID int64
	require.NoError(t, tx.QueryRowContext(ctx, `
INSERT INTO settlements (merchant_id, period_start, period_end, gross_cost, commission, net_payable, status)
VALUES ($1, $2, $3, 40, 4, 36, 'pending')
RETURNING id`, merchantID, from, to).Scan(&existingID))

	var wg sync.WaitGroup
	defer func() {
		_ = tx.Rollback()
		cancel()
		wg.Wait()
	}()

	type createResult struct {
		settlement *service.Settlement
		err        error
	}
	results := make(chan createResult, 2)
	start := make(chan struct{})

	for _, conn := range []*sql.Conn{conn2, conn3} {
		repo := newSettlementRepositoryWithSQL(nil, conn)
		wg.Add(1)
		go func(repo *settlementRepository) {
			defer wg.Done()
			<-start
			settlement, err := repo.CreateOrGet(ctx, &service.Settlement{
				MerchantID:  merchantID,
				PeriodStart: from,
				PeriodEnd:   to,
				GrossCost:   999, // 冲突时不得覆盖
				Commission:  999,
				NetPayable:  999,
				Status:      service.SettlementStatusPending,
			})
			results <- createResult{settlement: settlement, err: err}
		}(repo)
	}

	close(start)
	// 等两个调用都堵在未提交的唯一索引冲突上再提交，确保测试真的覆盖并发路径，
	// 而不是碰巧在 conn1 提交之后才开始执行。
	require.Eventually(t, func() bool {
		var waiting int
		err := integrationDB.QueryRowContext(ctx, `
SELECT COUNT(*) FROM pg_stat_activity
WHERE datname = current_database()
  AND pid <> pg_backend_pid()
  AND wait_event_type = 'Lock'
  AND query ILIKE '%INSERT INTO settlements%'`).Scan(&waiting)
		return err == nil && waiting >= 2
	}, 5*time.Second, 50*time.Millisecond)
	require.NoError(t, tx.Commit())

	for i := 0; i < 2; i++ {
		result := <-results
		require.NoError(t, result.err)
		require.NotNil(t, result.settlement)
		require.Equal(t, existingID, result.settlement.ID, "两次调用必须拿到同一行")
		require.InDelta(t, 36, result.settlement.NetPayable, 1e-9, "冲突时不得覆盖已封账金额")
	}

	var count int
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM settlements WHERE merchant_id = $1`, merchantID).Scan(&count))
	require.Equal(t, 1, count, "同一商户同一账期只能有一行")
}

// 迁移 256 必须为上线前已存在的结算单回填明细，且过滤条件与结算汇总一致：
// 只算 actual_cost > 0、billing_type = 0（用户实付）、commission_rate IS NOT NULL 的用量。
func TestSettlementRepositoryIntegrationMigration256BackfillsItems(t *testing.T) {
	ctx := context.Background()
	merchantID := time.Now().UnixNano()
	from := time.Now().UTC().Add(-10 * 24 * time.Hour).Truncate(time.Second)
	to := from.Add(24 * time.Hour)

	var userID, accountID, apiKeyID int64
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`INSERT INTO users (email, password_hash) VALUES ($1, 'x') RETURNING id`,
		fmt.Sprintf("settlement-backfill-%d@test.invalid", time.Now().UnixNano())).Scan(&userID))
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`INSERT INTO accounts (name, platform, type) VALUES ('settlement-backfill', 'openai', 'oauth') RETURNING id`).
		Scan(&accountID))
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`INSERT INTO api_keys (user_id, key, name) VALUES ($1, $2, 'settlement-backfill') RETURNING id`,
		userID, fmt.Sprintf("sk-settlement-backfill-%d", time.Now().UnixNano())).Scan(&apiKeyID))

	insertUsage := func(cost float64, commissionRate any, billingType int8, at time.Time) {
		_, err := integrationDB.ExecContext(ctx, `
INSERT INTO usage_logs (user_id, api_key_id, account_id, model, actual_cost, billing_type, merchant_id, commission_rate, created_at)
VALUES ($1, $2, $3, 'settlement-backfill', $4, $5, $6, $7, $8)`,
			userID, apiKeyID, accountID, cost, billingType, merchantID, commissionRate, at)
		require.NoError(t, err)
	}
	insertUsage(10, 0.1, service.BillingTypeBalance, from.Add(time.Hour)) // 唯一应回填的行
	insertUsage(5, nil, service.BillingTypeBalance, from.Add(2*time.Hour))
	insertUsage(7, 0.1, service.BillingTypeBalance+1, from.Add(3*time.Hour))
	insertUsage(0, 0.1, service.BillingTypeBalance, from.Add(4*time.Hour))

	var settlementID int64
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
INSERT INTO settlements (merchant_id, period_start, period_end, gross_cost, commission, net_payable, status)
VALUES ($1, $2, $3, 0, 0, 0, 'pending')
RETURNING id`, merchantID, from, to).Scan(&settlementID))

	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM settlement_items WHERE settlement_id = $1`, settlementID)
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM settlements WHERE id = $1`, settlementID)
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM usage_logs WHERE merchant_id = $1`, merchantID)
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM api_keys WHERE id = $1`, apiKeyID)
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM accounts WHERE id = $1`, accountID)
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, userID)
	})

	content, err := migrations.FS.ReadFile("256_settlement_ops.sql")
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(ctx, string(content))
	require.NoError(t, err, "迁移 256 应可重复执行并完成明细回填")
	_, err = integrationDB.ExecContext(ctx, string(content))
	require.NoError(t, err, "重复执行不得追加重复明细")

	var count int
	var totalCost, totalCommission float64
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
SELECT COUNT(*), COALESCE(SUM(cost), 0), COALESCE(SUM(commission), 0)
FROM settlement_items
WHERE settlement_id = $1`, settlementID).Scan(&count, &totalCost, &totalCommission))
	require.Equal(t, 1, count, "只回填可结算用量")
	require.InDelta(t, 10, totalCost, 1e-9)
	require.InDelta(t, 1, totalCommission, 1e-9)
}
