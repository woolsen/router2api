//go:build integration

package repository

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/woolsen/router2api/internal/service"
)

func cleanupDisputeIntegrationMerchant(t *testing.T, ctx context.Context, merchantID int64) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM disputes WHERE owner_type = 'merchant' AND owner_id = $1`, merchantID)
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM merchant_ledger WHERE merchant_id = $1`, merchantID)
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM merchant_balance WHERE merchant_id = $1`, merchantID)
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM settlements WHERE merchant_id = $1`, merchantID)
	})
}

func seedDisputeIntegrationSettlement(
	t *testing.T,
	ctx context.Context,
	merchantID int64,
	status string,
	netPayable float64,
) int64 {
	t.Helper()
	from := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
	to := from.Add(24 * time.Hour)
	var settlementID int64
	err := integrationDB.QueryRowContext(ctx, `
INSERT INTO settlements (merchant_id, period_start, period_end, gross_cost, commission, net_payable, status)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING id`,
		merchantID, from, to, netPayable+netPayable*0.1, netPayable*0.1, netPayable, status).Scan(&settlementID)
	require.NoError(t, err)
	return settlementID
}

func disputeIntegrationBalance(t *testing.T, ctx context.Context, merchantID int64) (balance, frozen float64) {
	t.Helper()
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT balance, frozen FROM merchant_balance WHERE merchant_id = $1`, merchantID).
		Scan(&balance, &frozen))
	return balance, frozen
}

func TestDisputeIntegrationRejectReleasesFreezeWithoutSecondSettle(t *testing.T) {
	ctx := context.Background()
	merchantID := int64(993001)
	cleanupDisputeIntegrationMerchant(t, ctx, merchantID)
	settlementID := seedDisputeIntegrationSettlement(t, ctx, merchantID, service.SettlementStatusPending, 80)

	settlementRepo := NewSettlementOpsRepository(integrationEntClient, integrationDB)
	tr, err := settlementRepo.ConfirmSettlement(ctx, settlementID, "integration@test", time.Now().UTC())
	require.NoError(t, err)
	require.True(t, tr.Applied)

	repo := NewDisputeRepository(integrationDB)
	d, err := repo.OpenSettlement(ctx, service.OpenDisputeInput{
		OwnerType: service.DisputeOwnerMerchant, OwnerID: merchantID,
		TargetType: service.DisputeTargetSettlement, TargetID: settlementID,
		Amount: 3, Reason: "amount mismatch",
	}, time.Now().UTC())
	require.NoError(t, err)
	require.Equal(t, service.DisputeStatusOpen, d.Status)

	balance, frozen := disputeIntegrationBalance(t, ctx, merchantID)
	require.InDelta(t, 0, balance, 1e-9)
	require.InDelta(t, 80, frozen, 1e-9)

	_, err = repo.Resolve(ctx, d.ID, service.ResolveInput{
		Decision: service.DisputeDecisionReject, By: "integration@test",
	}, time.Now().UTC())
	require.NoError(t, err)

	balance, frozen = disputeIntegrationBalance(t, ctx, merchantID)
	require.InDelta(t, 80, balance, 1e-9, "reject must release the freeze")
	require.InDelta(t, 0, frozen, 1e-9)

	var settleRows, releaseRows int
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM merchant_ledger WHERE type='settle' AND ref_type='settlement' AND ref_id=$1`,
		settlementID).Scan(&settleRows))
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM merchant_ledger WHERE type='dispute_release' AND ref_type='dispute' AND ref_id=$1`,
		d.ID).Scan(&releaseRows))
	require.Equal(t, 1, settleRows, "reject must never write a second settle")
	require.Equal(t, 1, releaseRows)
}

func TestDisputeIntegrationConfirmSourceACreditsExactlyOnce(t *testing.T) {
	ctx := context.Background()
	merchantID := int64(993002)
	cleanupDisputeIntegrationMerchant(t, ctx, merchantID)
	settlementID := seedDisputeIntegrationSettlement(t, ctx, merchantID, service.SettlementStatusPending, 80)

	repo := NewDisputeRepository(integrationDB)
	d, err := repo.OpenSettlement(ctx, service.OpenDisputeInput{
		OwnerType: service.DisputeOwnerMerchant, OwnerID: merchantID,
		TargetType: service.DisputeTargetSettlement, TargetID: settlementID,
		Amount: 3, Reason: "amount mismatch",
	}, time.Now().UTC())
	require.NoError(t, err)

	var balanceRows int
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM merchant_balance WHERE merchant_id = $1`, merchantID).Scan(&balanceRows))
	require.Zero(t, balanceRows, "pending source must not be credited on open")

	_, err = repo.Resolve(ctx, d.ID, service.ResolveInput{
		Decision: service.DisputeDecisionConfirm, By: "integration@test",
	}, time.Now().UTC())
	require.NoError(t, err)

	balance, frozen := disputeIntegrationBalance(t, ctx, merchantID)
	require.InDelta(t, 80, balance, 1e-9)
	require.InDelta(t, 0, frozen, 1e-9)

	_, err = repo.Resolve(ctx, d.ID, service.ResolveInput{
		Decision: service.DisputeDecisionConfirm, By: "integration@test",
	}, time.Now().UTC())
	require.ErrorIs(t, err, service.ErrDisputeAlreadyResolved)

	var settleRows int
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM merchant_ledger WHERE type='settle' AND ref_type='settlement' AND ref_id=$1`,
		settlementID).Scan(&settleRows))
	require.Equal(t, 1, settleRows, "source A must credit exactly once")

	balance, _ = disputeIntegrationBalance(t, ctx, merchantID)
	require.InDelta(t, 80, balance, 1e-9)
}

func TestDisputeIntegrationAdjustDeductsFrozenAndReferencesDispute(t *testing.T) {
	ctx := context.Background()
	merchantID := int64(993003)
	cleanupDisputeIntegrationMerchant(t, ctx, merchantID)
	settlementID := seedDisputeIntegrationSettlement(t, ctx, merchantID, service.SettlementStatusPending, 80)

	settlementRepo := NewSettlementOpsRepository(integrationEntClient, integrationDB)
	_, err := settlementRepo.ConfirmSettlement(ctx, settlementID, "integration@test", time.Now().UTC())
	require.NoError(t, err)

	repo := NewDisputeRepository(integrationDB)
	d, err := repo.OpenSettlement(ctx, service.OpenDisputeInput{
		OwnerType: service.DisputeOwnerMerchant, OwnerID: merchantID,
		TargetType: service.DisputeTargetSettlement, TargetID: settlementID,
		Amount: 3, Reason: "amount mismatch",
	}, time.Now().UTC())
	require.NoError(t, err)

	_, err = repo.Resolve(ctx, d.ID, service.ResolveInput{
		Decision: service.DisputeDecisionAdjust, AdjustAmount: -3,
		Note: "核对误差", By: "integration@test",
	}, time.Now().UTC())
	require.NoError(t, err)

	balance, frozen := disputeIntegrationBalance(t, ctx, merchantID)
	require.InDelta(t, 77, balance, 1e-9)
	require.InDelta(t, 0, frozen, 1e-9)

	var amount float64
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT amount FROM merchant_ledger WHERE type='adjust' AND ref_type='dispute' AND ref_id=$1`,
		d.ID).Scan(&amount))
	require.InDelta(t, -3, amount, 1e-9)
}

func TestDisputeIntegrationWindowClosedRejected(t *testing.T) {
	ctx := context.Background()
	merchantID := int64(993004)
	cleanupDisputeIntegrationMerchant(t, ctx, merchantID)
	settlementID := seedDisputeIntegrationSettlement(t, ctx, merchantID, service.SettlementStatusPending, 80)

	_, err := integrationDB.ExecContext(ctx,
		`UPDATE settlements SET dispute_deadline = NOW() - INTERVAL '1 minute' WHERE id = $1`, settlementID)
	require.NoError(t, err)

	repo := NewDisputeRepository(integrationDB)
	_, err = repo.OpenSettlement(ctx, service.OpenDisputeInput{
		OwnerType: service.DisputeOwnerMerchant, OwnerID: merchantID,
		TargetType: service.DisputeTargetSettlement, TargetID: settlementID,
		Amount: 3, Reason: "late",
	}, time.Now().UTC())
	require.ErrorIs(t, err, service.ErrDisputeWindowClosed)

	var status string
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT status FROM settlements WHERE id = $1`, settlementID).Scan(&status))
	require.Equal(t, service.SettlementStatusPending, status)

	var disputes int
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM disputes WHERE target_type='settlement' AND target_id=$1`, settlementID).Scan(&disputes))
	require.Zero(t, disputes)
}

func TestDisputeIntegrationUsageRequiresOwner(t *testing.T) {
	ctx := context.Background()
	merchantID := int64(993005)
	cleanupDisputeIntegrationMerchant(t, ctx, merchantID)

	var userID, apiKeyID, accountID int64
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`INSERT INTO users (email, password_hash) VALUES ($1, 'x') RETURNING id`,
		fmt.Sprintf("dispute-user-%d@test.invalid", time.Now().UnixNano())).Scan(&userID))
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`INSERT INTO accounts (name, platform, type) VALUES ('dispute-test', 'openai', 'oauth') RETURNING id`).Scan(&accountID))
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`INSERT INTO api_keys (user_id, key, name) VALUES ($1, $2, 'dispute-test') RETURNING id`,
		userID, fmt.Sprintf("sk-dispute-%d", time.Now().UnixNano())).Scan(&apiKeyID))
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM disputes WHERE owner_type='user' AND owner_id=$1`, userID)
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM usage_logs WHERE user_id = $1`, userID)
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM api_keys WHERE id = $1`, apiKeyID)
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM accounts WHERE id = $1`, accountID)
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, userID)
	})

	var usageLogID int64
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
INSERT INTO usage_logs (user_id, api_key_id, account_id, model, actual_cost, billing_type, created_at)
VALUES ($1, $2, $3, 'dispute-test', 1, $4, NOW())
RETURNING id`, userID, apiKeyID, accountID, service.BillingTypeBalance).Scan(&usageLogID))

	repo := NewDisputeRepository(integrationDB)
	_, err := repo.OpenUsage(ctx, service.OpenDisputeInput{
		OwnerType: service.DisputeOwnerUser, OwnerID: userID + 1,
		TargetType: service.DisputeTargetUsage, TargetID: usageLogID,
		Amount: 1, Reason: "not mine",
	}, time.Now().UTC())
	require.True(t, errors.Is(err, service.ErrDisputeTargetNotFound))

	d, err := repo.OpenUsage(ctx, service.OpenDisputeInput{
		OwnerType: service.DisputeOwnerUser, OwnerID: userID,
		TargetType: service.DisputeTargetUsage, TargetID: usageLogID,
		Amount: 1, Reason: "double charged",
	}, time.Now().UTC())
	require.NoError(t, err)
	require.Equal(t, service.DisputeStatusOpen, d.Status)
}
