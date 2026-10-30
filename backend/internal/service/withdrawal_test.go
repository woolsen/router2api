package service

import (
	"context"
	"math"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeWithdrawalRepo 是 WithdrawalRepository 的内存实现。
// 只模拟仓储必须提供的原子语义（余额锁、单笔在途、流水唯一键）；
// 真实 SQL、行锁与部分唯一索引由 repository 层测试钉住。
type fakeWithdrawalRepo struct {
	balances    map[int64]float64
	frozen      map[int64]float64
	withdrawals map[int64]*Withdrawal
	ledger      map[string]float64

	nextID int64
}

func newFakeWithdrawalRepo() *fakeWithdrawalRepo {
	return &fakeWithdrawalRepo{
		balances:    make(map[int64]float64),
		frozen:      make(map[int64]float64),
		withdrawals: make(map[int64]*Withdrawal),
		ledger:      make(map[string]float64),
	}
}

func (r *fakeWithdrawalRepo) seedBalance(merchantID int64, balance float64) {
	r.balances[merchantID] = balance
}

func (r *fakeWithdrawalRepo) Available(_ context.Context, merchantID int64) (float64, error) {
	return r.balances[merchantID], nil
}

func (r *fakeWithdrawalRepo) List(_ context.Context, merchantID int64) ([]Withdrawal, error) {
	var out []Withdrawal
	for _, w := range r.withdrawals {
		if w.MerchantID == merchantID {
			out = append(out, *w)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}

func (r *fakeWithdrawalRepo) ListAll(_ context.Context, status string) ([]Withdrawal, error) {
	var out []Withdrawal
	for _, w := range r.withdrawals {
		if status == "" || w.Status == status {
			out = append(out, *w)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}

func (r *fakeWithdrawalRepo) Request(_ context.Context, merchantID int64, amount float64, at time.Time) (*Withdrawal, error) {
	if r.balances[merchantID]+1e-9 < amount {
		return nil, ErrAmountExceedsAvailable
	}
	for _, w := range r.withdrawals {
		if w.MerchantID == merchantID && (w.Status == WithdrawalStatusRequested || w.Status == WithdrawalStatusApproved) {
			return nil, ErrWithdrawalInFlight
		}
	}

	r.nextID++
	w := &Withdrawal{
		ID:          r.nextID,
		MerchantID:  merchantID,
		Amount:      amount,
		Status:      WithdrawalStatusRequested,
		RequestedAt: at,
	}
	r.withdrawals[w.ID] = w
	r.balances[merchantID] -= amount
	r.frozen[merchantID] += amount
	r.ledger[r.ledgerKey("withdraw", w.ID)] = -amount
	cp := *w
	return &cp, nil
}

func (r *fakeWithdrawalRepo) Approve(_ context.Context, withdrawalID int64, by string, _ time.Time) error {
	w, ok := r.withdrawals[withdrawalID]
	if !ok {
		return ErrWithdrawalNotFound
	}
	switch w.Status {
	case WithdrawalStatusRequested:
		w.Status = WithdrawalStatusApproved
		w.ApprovedBy = by
		return nil
	case WithdrawalStatusApproved:
		return nil
	case WithdrawalStatusPaid:
		return ErrAlreadyPaid
	default:
		return ErrWithdrawalStatusTransition
	}
}

func (r *fakeWithdrawalRepo) Reject(_ context.Context, withdrawalID int64, by string, _ time.Time) error {
	w, ok := r.withdrawals[withdrawalID]
	if !ok {
		return ErrWithdrawalNotFound
	}
	switch w.Status {
	case WithdrawalStatusRejected:
		return nil
	case WithdrawalStatusPaid:
		return ErrAlreadyPaid
	}

	w.Status = WithdrawalStatusRejected
	w.ApprovedBy = by
	key := r.ledgerKey("withdraw_reject", w.ID)
	if _, exists := r.ledger[key]; !exists {
		r.ledger[key] = w.Amount
		r.balances[w.MerchantID] += w.Amount
		r.frozen[w.MerchantID] -= w.Amount
	}
	return nil
}

func (r *fakeWithdrawalRepo) MarkPaid(_ context.Context, withdrawalID int64, proofPath, sha256, by string, at time.Time) error {
	w, ok := r.withdrawals[withdrawalID]
	if !ok {
		return ErrWithdrawalNotFound
	}
	if w.Status != WithdrawalStatusApproved {
		return ErrAlreadyPaid
	}

	w.Status = WithdrawalStatusPaid
	w.ProofPath = proofPath
	w.ProofSHA256 = sha256
	w.ApprovedBy = by
	paidAt := at
	w.PaidAt = &paidAt

	key := r.ledgerKey("withdraw_paid", w.ID)
	if _, exists := r.ledger[key]; !exists {
		r.ledger[key] = 0
		r.frozen[w.MerchantID] -= w.Amount
	}
	return nil
}

func (r *fakeWithdrawalRepo) ledgerKey(kind string, id int64) string {
	return kind + "|withdrawal|" + strconv.FormatInt(id, 10)
}

func newWithdrawalTestService() (*WithdrawalService, *fakeWithdrawalRepo) {
	repo := newFakeWithdrawalRepo()
	return NewWithdrawalService(repo), repo
}

func TestAvailableReadsWalletBalanceNotPendingSettlements(t *testing.T) {
	svc, repo := newWithdrawalTestService()
	// 钱包余额只由已确认结算入账；pending 结算从未进 balance，因此可用额就是 100。
	repo.seedBalance(1, 100)

	available, err := svc.Available(context.Background(), 1)
	require.NoError(t, err)
	require.InDelta(t, 100, available, 1e-9)
}

func TestAvailableMissingBalanceIsZero(t *testing.T) {
	svc, _ := newWithdrawalTestService()

	available, err := svc.Available(context.Background(), 42)
	require.NoError(t, err)
	require.Zero(t, available)
}

func TestRequestBelowMinimumRejected(t *testing.T) {
	svc, repo := newWithdrawalTestService()
	repo.seedBalance(1, 100)

	_, err := svc.Request(context.Background(), 1, 49.99)
	require.ErrorIs(t, err, ErrBelowMinimum)
	require.InDelta(t, 100, repo.balances[1], 1e-9)
	require.Empty(t, repo.withdrawals)
}

func TestRequestAtMinimumAccepted(t *testing.T) {
	svc, repo := newWithdrawalTestService()
	repo.seedBalance(1, 50)

	w, err := svc.Request(context.Background(), 1, 50)
	require.NoError(t, err)
	require.Equal(t, WithdrawalStatusRequested, w.Status)
	require.InDelta(t, 50, w.Amount, 1e-9)
	require.InDelta(t, 0, repo.balances[1], 1e-9)
	require.InDelta(t, 50, repo.frozen[1], 1e-9)
}

func TestRequestTwiceDoesNotDoubleSpend(t *testing.T) {
	svc, repo := newWithdrawalTestService()
	repo.seedBalance(1, 100)

	_, err := svc.Request(context.Background(), 1, 100)
	require.NoError(t, err)

	_, err = svc.Request(context.Background(), 1, 100)
	require.ErrorIs(t, err, ErrAmountExceedsAvailable)
	require.Len(t, repo.withdrawals, 1)
	require.InDelta(t, 0, repo.balances[1], 1e-9)
	require.InDelta(t, 100, repo.frozen[1], 1e-9)
}

func TestRequestSecondInFlightRejectedEvenWhenFundsRemain(t *testing.T) {
	svc, repo := newWithdrawalTestService()
	repo.seedBalance(1, 200)

	_, err := svc.Request(context.Background(), 1, 100)
	require.NoError(t, err)

	_, err = svc.Request(context.Background(), 1, 100)
	require.ErrorIs(t, err, ErrWithdrawalInFlight)
	require.Len(t, repo.withdrawals, 1)
	require.InDelta(t, 100, repo.balances[1], 1e-9)
	require.InDelta(t, 100, repo.frozen[1], 1e-9)
}

func TestRequestRejectsNonFiniteAmount(t *testing.T) {
	svc, repo := newWithdrawalTestService()
	repo.seedBalance(1, 100)

	for _, amount := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		_, err := svc.Request(context.Background(), 1, amount)
		require.ErrorIs(t, err, ErrWithdrawalAmountInvalid)
	}
	require.Empty(t, repo.withdrawals)
}

func TestMarkPaidTwiceSecondFails(t *testing.T) {
	svc, repo := newWithdrawalTestService()
	repo.seedBalance(1, 100)

	w, err := svc.Request(context.Background(), 1, 100)
	require.NoError(t, err)
	require.NoError(t, svc.Approve(context.Background(), w.ID, "ops"))

	require.NoError(t, svc.MarkPaid(context.Background(), w.ID, "proof.png", "abc123", "ops"))
	require.InDelta(t, 0, repo.balances[1], 1e-9)
	require.InDelta(t, 0, repo.frozen[1], 1e-9)

	err = svc.MarkPaid(context.Background(), w.ID, "proof.png", "abc123", "ops")
	require.ErrorIs(t, err, ErrAlreadyPaid)
	require.InDelta(t, 0, repo.balances[1], 1e-9)
	require.InDelta(t, 0, repo.frozen[1], 1e-9)
	require.Equal(t, WithdrawalStatusPaid, repo.withdrawals[w.ID].Status)
}

func TestMarkPaidRequiresApproved(t *testing.T) {
	svc, repo := newWithdrawalTestService()
	repo.seedBalance(1, 100)

	w, err := svc.Request(context.Background(), 1, 100)
	require.NoError(t, err)

	err = svc.MarkPaid(context.Background(), w.ID, "proof.png", "abc123", "ops")
	require.ErrorIs(t, err, ErrAlreadyPaid)
	require.Equal(t, WithdrawalStatusRequested, repo.withdrawals[w.ID].Status)
	require.InDelta(t, 100, repo.frozen[1], 1e-9)
}

func TestRejectUnfreezesBalance(t *testing.T) {
	svc, repo := newWithdrawalTestService()
	repo.seedBalance(1, 100)

	w, err := svc.Request(context.Background(), 1, 100)
	require.NoError(t, err)
	require.NoError(t, svc.Reject(context.Background(), w.ID, "ops"))

	require.Equal(t, WithdrawalStatusRejected, repo.withdrawals[w.ID].Status)
	require.InDelta(t, 100, repo.balances[1], 1e-9)
	require.InDelta(t, 0, repo.frozen[1], 1e-9)

	// 重复拒绝幂等：不能二次退款。
	require.NoError(t, svc.Reject(context.Background(), w.ID, "ops"))
	require.InDelta(t, 100, repo.balances[1], 1e-9)
	require.InDelta(t, 0, repo.frozen[1], 1e-9)
}

func TestRejectPaidFails(t *testing.T) {
	svc, repo := newWithdrawalTestService()
	repo.seedBalance(1, 100)

	w, err := svc.Request(context.Background(), 1, 100)
	require.NoError(t, err)
	require.NoError(t, svc.Approve(context.Background(), w.ID, "ops"))
	require.NoError(t, svc.MarkPaid(context.Background(), w.ID, "proof.png", "abc123", "ops"))

	err = svc.Reject(context.Background(), w.ID, "ops")
	require.ErrorIs(t, err, ErrAlreadyPaid)
	require.InDelta(t, 0, repo.frozen[1], 1e-9)
}

func TestApproveIsIdempotent(t *testing.T) {
	svc, repo := newWithdrawalTestService()
	repo.seedBalance(1, 100)

	w, err := svc.Request(context.Background(), 1, 100)
	require.NoError(t, err)

	require.NoError(t, svc.Approve(context.Background(), w.ID, "ops"))
	require.NoError(t, svc.Approve(context.Background(), w.ID, "ops"))
	require.Equal(t, WithdrawalStatusApproved, repo.withdrawals[w.ID].Status)
}
