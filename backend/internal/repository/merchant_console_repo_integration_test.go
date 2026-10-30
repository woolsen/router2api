//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/woolsen/router2api/internal/service"
)

// 结算口径要求 gross 与 commission + net_payable 使用同一批可结算行。
// 这里真的写一条 commission_rate IS NULL 的用量，防止 gross 把未知费率行算进去。
func TestMerchantConsoleRepositoryDailyUsageExcludesUnknownCommissionRows(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)

	user := mustCreateUser(t, client, &service.User{Email: "merchant-usage-null-rate@example.com"})
	key := mustCreateApiKey(t, client, &service.APIKey{UserID: user.ID})
	account := mustCreateAccount(t, client, &service.Account{Name: "merchant-usage-null-rate"})

	var merchantID int64
	err := integrationDB.QueryRowContext(ctx,
		`INSERT INTO merchants (name, commission_rate) VALUES ('usage-null-rate', 0.1000) RETURNING id`,
	).Scan(&merchantID)
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(ctx, "DELETE FROM usage_logs WHERE user_id = $1", user.ID)
		_, _ = integrationDB.ExecContext(ctx, "DELETE FROM merchants WHERE id = $1", merchantID)
		_, _ = integrationDB.ExecContext(ctx, "DELETE FROM api_keys WHERE id = $1", key.ID)
		_, _ = integrationDB.ExecContext(ctx, "DELETE FROM accounts WHERE id = $1", account.ID)
		_, _ = integrationDB.ExecContext(ctx, "DELETE FROM users WHERE id = $1", user.ID)
	})

	day := time.Now().UTC().Truncate(24 * time.Hour)
	at := day.Add(12 * time.Hour)
	insert := func(actualCost string, commissionRate any, billingType int8) {
		t.Helper()
		_, err := integrationDB.ExecContext(ctx, `INSERT INTO usage_logs
   (user_id, api_key_id, account_id, model, actual_cost, billing_type, merchant_id, commission_rate, created_at)
   VALUES ($1, $2, $3, 'gpt-test', $4, $5, $6, $7, $8)`,
			user.ID, key.ID, account.ID, actualCost, billingType, merchantID, commissionRate, at)
		require.NoError(t, err)
	}

	insert("1.0000000000", 0.1, service.BillingTypeBalance)
	insert("2.0000000000", 0.1, service.BillingTypeBalance)
	// 费率未知：金额不能计入 gross/commission/net，也不能冒充 0 抽成。
	insert("5.0000000000", nil, service.BillingTypeBalance)
	// 订阅额度不是用户实付，整行排除。
	insert("4.0000000000", 0.1, service.BillingTypeSubscription)

	repo := newMerchantConsoleRepositoryWithSQL(integrationDB)
	points, err := repo.DailyUsage(ctx, merchantID, day, day.Add(24*time.Hour))
	require.NoError(t, err)
	require.Len(t, points, 1)

	point := points[0]
	require.Equal(t, day.Format("2006-01-02"), point.Date)
	require.InDelta(t, 3.0, point.GrossCost, 1e-9)
	require.InDelta(t, 0.3, point.Commission, 1e-9)
	require.InDelta(t, 2.7, point.NetPayable, 1e-9)
	require.InDelta(t, point.GrossCost, point.Commission+point.NetPayable, 1e-9)
}
