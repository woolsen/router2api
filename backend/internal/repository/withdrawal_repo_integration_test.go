//go:build integration

package repository

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/woolsen/router2api/internal/service"
)

func cleanupWithdrawalIntegrationMerchant(t *testing.T, ctx context.Context, merchantID int64) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM merchant_ledger WHERE merchant_id = $1`, merchantID)
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM withdrawals WHERE merchant_id = $1`, merchantID)
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM merchant_balance WHERE merchant_id = $1`, merchantID)
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM settlements WHERE merchant_id = $1`, merchantID)
	})
}

func seedWithdrawalIntegrationBalance(t *testing.T, ctx context.Context, merchantID int64, balance float64) {
	t.Helper()
	_, err := integrationDB.ExecContext(ctx, `
INSERT INTO merchant_balance (merchant_id, balance, frozen) VALUES ($1, $2, 0)
ON CONFLICT (merchant_id) DO UPDATE SET balance = EXCLUDED.balance, frozen = 0`,
		merchantID, balance)
	require.NoError(t, err)
}

func withdrawalIntegrationBalance(t *testing.T, ctx context.Context, merchantID int64) (balance, frozen float64) {
	t.Helper()
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT balance, frozen FROM merchant_balance WHERE merchant_id = $1`, merchantID).
		Scan(&balance, &frozen))
	return balance, frozen
}

func TestWithdrawalIntegrationConcurrentRequestsOnlyOneInFlight(t *testing.T) {
	ctx := context.Background()
	merchantID := int64(992001)
	cleanupWithdrawalIntegrationMerchant(t, ctx, merchantID)
	seedWithdrawalIntegrationBalance(t, ctx, merchantID, 100)

	repo := NewWithdrawalRepository(integrationDB)
	svc := service.NewWithdrawalService(repo)

	start := make(chan struct{})
	type result struct {
		w   *service.Withdrawal
		err error
	}
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			w, err := svc.Request(ctx, merchantID, 100)
			results <- result{w: w, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	var success, rejected int
	for res := range results {
		if res.err == nil {
			success++
			require.NotNil(t, res.w)
			continue
		}
		rejected++
		require.True(t,
			errors.Is(res.err, service.ErrAmountExceedsAvailable) ||
				errors.Is(res.err, service.ErrWithdrawalInFlight),
			"unexpected error: %v", res.err)
	}
	require.Equal(t, 1, success, "exactly one request may freeze funds")
	require.Equal(t, 1, rejected)

	balance, frozen := withdrawalIntegrationBalance(t, ctx, merchantID)
	require.InDelta(t, 0, balance, 1e-9)
	require.InDelta(t, 100, frozen, 1e-9)

	var withdrawals, ledger int
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM withdrawals WHERE merchant_id = $1`, merchantID).Scan(&withdrawals))
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM merchant_ledger WHERE merchant_id = $1`, merchantID).Scan(&ledger))
	require.Equal(t, 1, withdrawals)
	require.Equal(t, 1, ledger)
}

func TestWithdrawalIntegrationPayIsConditionalAndReleasesFrozenOnce(t *testing.T) {
	ctx := context.Background()
	merchantID := int64(992002)
	cleanupWithdrawalIntegrationMerchant(t, ctx, merchantID)
	seedWithdrawalIntegrationBalance(t, ctx, merchantID, 200)

	repo := NewWithdrawalRepository(integrationDB)
	svc := service.NewWithdrawalService(repo)

	w, err := svc.Request(ctx, merchantID, 100)
	require.NoError(t, err)

	all, err := repo.ListAll(ctx, "")
	require.NoError(t, err)
	require.Len(t, all, 1)
	pending, err := repo.ListAll(ctx, service.WithdrawalStatusRequested)
	require.NoError(t, err)
	require.Len(t, pending, 1)

	// 未审核前打款必须失败：MarkPaid 只接受 approved。
	err = svc.MarkPaid(ctx, w.ID, "proof.png", "sha", "ops@example.com")
	require.ErrorIs(t, err, service.ErrAlreadyPaid)
	require.NoError(t, svc.Approve(ctx, w.ID, "ops@example.com"))

	// 在途提现未结束前，即使余额仍足够也不能再发起第二笔。
	_, err = svc.Request(ctx, merchantID, 100)
	require.ErrorIs(t, err, service.ErrWithdrawalInFlight)

	require.NoError(t, svc.MarkPaid(ctx, w.ID, "proof.png", "sha", "ops@example.com"))
	err = svc.MarkPaid(ctx, w.ID, "proof.png", "sha", "ops@example.com")
	require.ErrorIs(t, err, service.ErrAlreadyPaid)

	balance, frozen := withdrawalIntegrationBalance(t, ctx, merchantID)
	require.InDelta(t, 100, balance, 1e-9)
	require.InDelta(t, 0, frozen, 1e-9)

	var status string
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT status FROM withdrawals WHERE id = $1`, w.ID).Scan(&status))
	require.Equal(t, service.WithdrawalStatusPaid, status)

	var paidRows int
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM merchant_ledger WHERE merchant_id = $1 AND type = 'withdraw_paid'`, merchantID).
		Scan(&paidRows))
	require.Equal(t, 1, paidRows)
}

func TestWithdrawalIntegrationRejectUnfreezesAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	merchantID := int64(992003)
	cleanupWithdrawalIntegrationMerchant(t, ctx, merchantID)
	seedWithdrawalIntegrationBalance(t, ctx, merchantID, 100)

	repo := NewWithdrawalRepository(integrationDB)
	svc := service.NewWithdrawalService(repo)

	w, err := svc.Request(ctx, merchantID, 60)
	require.NoError(t, err)
	require.NoError(t, svc.Reject(ctx, w.ID, "ops@example.com"))
	require.NoError(t, svc.Reject(ctx, w.ID, "ops@example.com"))

	balance, frozen := withdrawalIntegrationBalance(t, ctx, merchantID)
	require.InDelta(t, 100, balance, 1e-9)
	require.InDelta(t, 0, frozen, 1e-9)

	var rejectRows int
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM merchant_ledger WHERE merchant_id = $1 AND type = 'withdraw_reject'`, merchantID).
		Scan(&rejectRows))
	require.Equal(t, 1, rejectRows)
}

func TestWithdrawalIntegrationAvailableIgnoresPendingSettlements(t *testing.T) {
	ctx := context.Background()
	merchantID := int64(992004)
	cleanupWithdrawalIntegrationMerchant(t, ctx, merchantID)
	seedWithdrawalIntegrationBalance(t, ctx, merchantID, 100)

	now := time.Now().UTC().Truncate(time.Second)
	_, err := integrationDB.ExecContext(ctx, `
INSERT INTO settlements (merchant_id, period_start, period_end, gross_cost, commission, net_payable, status)
VALUES ($1, $2, $3, 500, 50, 450, 'pending')`,
		merchantID, now.Add(-7*24*time.Hour), now)
	require.NoError(t, err)

	repo := NewWithdrawalRepository(integrationDB)
	available, err := repo.Available(ctx, merchantID)
	require.NoError(t, err)
	require.InDelta(t, 100, available, 1e-9,
		"pending settlements never enter merchant_balance.balance and must not change available funds")
}
