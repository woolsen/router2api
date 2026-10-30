package service

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeSettlementOpsRepo 是 SettlementOpsRepository 的内存实现。
// 只模拟仓储的原子操作（条件更新、余额入账、流水唯一键），
// 真实 SQL 的过滤与事务语义由 repository 层测试钉住。
type fakeSettlementOpsRepo struct {
	settlements map[int64]*Settlement
	items       map[int64][]SettlementItem
	balances    map[int64]float64
	frozen      map[int64]float64
	ledger      map[string]struct{}

	nextID       int64
	confirmCalls int
	disputeCalls int
}

func newFakeSettlementOpsRepo() *fakeSettlementOpsRepo {
	return &fakeSettlementOpsRepo{
		settlements: make(map[int64]*Settlement),
		items:       make(map[int64][]SettlementItem),
		balances:    make(map[int64]float64),
		frozen:      make(map[int64]float64),
		ledger:      make(map[string]struct{}),
	}
}

func (r *fakeSettlementOpsRepo) seedSettlement(createdAt time.Time, status string, netPayable float64) *Settlement {
	r.nextID++
	deadline := createdAt.Add(SettlementDisputeWindow)
	s := &Settlement{
		ID:              r.nextID,
		MerchantID:      1,
		PeriodStart:     createdAt.Add(-settlementWeek),
		PeriodEnd:       createdAt,
		GrossCost:       netPayable,
		NetPayable:      netPayable,
		Status:          status,
		DisputeDeadline: &deadline,
	}
	r.settlements[s.ID] = s
	return s
}

func (r *fakeSettlementOpsRepo) seedItem(settlementID, usageLogID int64, cost, commission float64) {
	r.items[settlementID] = append(r.items[settlementID], SettlementItem{
		ID:           int64(len(r.items[settlementID]) + 1),
		SettlementID: settlementID,
		UsageLogID:   usageLogID,
		Cost:         cost,
		Commission:   commission,
	})
}

func (r *fakeSettlementOpsRepo) GetSettlement(_ context.Context, settlementID int64) (*Settlement, error) {
	s, ok := r.settlements[settlementID]
	if !ok {
		return nil, ErrSettlementNotFound
	}
	cp := *s
	return &cp, nil
}

func (r *fakeSettlementOpsRepo) ListSettlementItems(_ context.Context, settlementID int64) ([]SettlementItem, error) {
	return append([]SettlementItem(nil), r.items[settlementID]...), nil
}

func (r *fakeSettlementOpsRepo) ListExpiredPendingSettlements(
	_ context.Context,
	before time.Time,
	afterID int64,
	limit int,
) ([]*Settlement, error) {
	var pending []*Settlement
	for _, s := range r.settlements {
		if s.Status != SettlementStatusPending || s.ID <= afterID {
			continue
		}
		if s.DisputeDeadline == nil || !s.DisputeDeadline.Before(before) {
			continue
		}
		cp := *s
		pending = append(pending, &cp)
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].ID < pending[j].ID })
	if limit > 0 && len(pending) > limit {
		pending = pending[:limit]
	}
	return pending, nil
}

func (r *fakeSettlementOpsRepo) ConfirmSettlement(_ context.Context, settlementID int64, _ string, at time.Time) (SettlementTransition, error) {
	r.confirmCalls++
	s, ok := r.settlements[settlementID]
	if !ok {
		return SettlementTransition{}, ErrSettlementNotFound
	}
	if s.Status != SettlementStatusPending {
		return SettlementTransition{Applied: false, Status: s.Status}, nil
	}

	confirmedAt := at
	s.Status = SettlementStatusConfirmed
	s.ConfirmedAt = &confirmedAt
	key := "settle|settlement|" + strconv.FormatInt(settlementID, 10)
	if _, exists := r.ledger[key]; !exists {
		r.ledger[key] = struct{}{}
		r.balances[s.MerchantID] += s.NetPayable
	}
	return SettlementTransition{Applied: true, Status: SettlementStatusConfirmed}, nil
}

func (r *fakeSettlementOpsRepo) DisputeSettlement(_ context.Context, settlementID int64, _ string, at time.Time) (SettlementTransition, error) {
	r.disputeCalls++
	s, ok := r.settlements[settlementID]
	if !ok {
		return SettlementTransition{}, ErrSettlementNotFound
	}
	if s.Status == SettlementStatusPaid || s.Status == SettlementStatusDisputed {
		return SettlementTransition{Applied: false, Status: s.Status}, nil
	}
	if s.DisputeDeadline != nil && !at.Before(*s.DisputeDeadline) {
		return SettlementTransition{Applied: false, Status: s.Status, DisputeWindowClosed: true}, nil
	}

	wasConfirmed := s.Status == SettlementStatusConfirmed
	s.Status = SettlementStatusDisputed
	if wasConfirmed {
		key := "dispute_hold|settlement|" + strconv.FormatInt(settlementID, 10)
		if _, exists := r.ledger[key]; !exists {
			r.ledger[key] = struct{}{}
			r.balances[s.MerchantID] -= s.NetPayable
			r.frozen[s.MerchantID] += s.NetPayable
		}
	}
	return SettlementTransition{Applied: true, Status: SettlementStatusDisputed}, nil
}

func newOpsTest(t *testing.T, now time.Time) (*SettlementOps, *fakeSettlementOpsRepo) {
	t.Helper()
	repo := newFakeSettlementOpsRepo()
	ops := NewSettlementOps(repo)
	ops.now = func() time.Time { return now }
	return ops, repo
}

func TestAutoConfirmOnlyAfterSevenDays(t *testing.T) {
	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	ops, repo := newOpsTest(t, now)
	old := repo.seedSettlement(now.Add(-8*24*time.Hour), SettlementStatusPending, 100)
	fresh := repo.seedSettlement(now.Add(-3*24*time.Hour), SettlementStatusPending, 50)

	n, err := ops.AutoConfirmExpired(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, n, "only the settlement past its 7-day deadline may be confirmed")

	require.Equal(t, SettlementStatusConfirmed, repo.settlements[old.ID].Status)
	require.Equal(t, SettlementStatusPending, repo.settlements[fresh.ID].Status)
	require.InDelta(t, 100, repo.balances[old.MerchantID], 1e-9)
	_, freshCredited := repo.ledger["settle|settlement|"+strconv.FormatInt(fresh.ID, 10)]
	require.False(t, freshCredited, "fresh settlement must not be credited")
}

func TestDisputedSettlementCannotBeConfirmed(t *testing.T) {
	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	ops, repo := newOpsTest(t, now)
	s := repo.seedSettlement(now.Add(-30*24*time.Hour), SettlementStatusDisputed, 80)

	err := ops.Confirm(context.Background(), s.ID, "cron")
	require.ErrorIs(t, err, ErrSettlementDisputed)
	require.Equal(t, SettlementStatusDisputed, repo.settlements[s.ID].Status)
	require.Zero(t, repo.balances[s.MerchantID], "disputed settlement must not credit balance")
	require.Empty(t, repo.ledger)
}

func TestConfirmCreditsBalanceExactlyOnce(t *testing.T) {
	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	ops, repo := newOpsTest(t, now)
	s := repo.seedSettlement(now.Add(-2*24*time.Hour), SettlementStatusPending, 88.5)

	require.NoError(t, ops.Confirm(context.Background(), s.ID, "ops@example.com"))
	require.NoError(t, ops.Confirm(context.Background(), s.ID, "ops@example.com"), "confirm must be idempotent")

	require.Equal(t, SettlementStatusConfirmed, repo.settlements[s.ID].Status)
	require.NotNil(t, repo.settlements[s.ID].ConfirmedAt)
	require.InDelta(t, 88.5, repo.balances[s.MerchantID], 1e-9)
	require.Len(t, repo.ledger, 1, "a settlement must produce exactly one settlement ledger row")
}

func TestPaidSettlementCannotBeConfirmedAgain(t *testing.T) {
	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	ops, repo := newOpsTest(t, now)
	s := repo.seedSettlement(now.Add(-30*24*time.Hour), SettlementStatusPaid, 80)

	err := ops.Confirm(context.Background(), s.ID, "ops")
	require.ErrorIs(t, err, ErrSettlementPaid)
	require.Zero(t, repo.balances[s.MerchantID], "paid settlement must never be re-credited")
	require.Empty(t, repo.ledger)
}

func TestDisputeMovesSettlementAndBlocksConfirm(t *testing.T) {
	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	ops, repo := newOpsTest(t, now)
	s := repo.seedSettlement(now.Add(-2*24*time.Hour), SettlementStatusPending, 10)

	require.NoError(t, ops.Dispute(context.Background(), s.ID, "amount mismatch"))
	require.Equal(t, SettlementStatusDisputed, repo.settlements[s.ID].Status)

	err := ops.Confirm(context.Background(), s.ID, "ops")
	require.ErrorIs(t, err, ErrSettlementDisputed)
}

func TestSettlementDisputePendingDoesNotFreeze(t *testing.T) {
	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	ops, repo := newOpsTest(t, now)
	s := repo.seedSettlement(now.Add(-2*24*time.Hour), SettlementStatusPending, 10)

	require.NoError(t, ops.Dispute(context.Background(), s.ID, "amount mismatch"))
	require.Zero(t, repo.balances[s.MerchantID], "pending 结算单从未入账，申诉时不应冻结")
	require.Zero(t, repo.frozen[s.MerchantID])
	require.Empty(t, repo.ledger)
}

func TestSettlementDisputeConfirmedFreezesCreditedBalance(t *testing.T) {
	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	ops, repo := newOpsTest(t, now)
	s := repo.seedSettlement(now.Add(-2*24*time.Hour), SettlementStatusConfirmed, 80)
	repo.balances[s.MerchantID] = 80 // 已确认入账

	require.NoError(t, ops.Dispute(context.Background(), s.ID, "amount mismatch"))
	require.Equal(t, SettlementStatusDisputed, repo.settlements[s.ID].Status)
	require.InDelta(t, 0, repo.balances[s.MerchantID], 1e-9)
	require.InDelta(t, 80, repo.frozen[s.MerchantID], 1e-9)
	require.Contains(t, repo.ledger, "dispute_hold|settlement|"+strconv.FormatInt(s.ID, 10))
}

func TestSettlementDisputeConfirmedAllowsNegativeBalance(t *testing.T) {
	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	ops, repo := newOpsTest(t, now)
	s := repo.seedSettlement(now.Add(-2*24*time.Hour), SettlementStatusConfirmed, 80)
	repo.balances[s.MerchantID] = 50 // 已提现一部分

	require.NoError(t, ops.Dispute(context.Background(), s.ID, "amount mismatch"))
	require.InDelta(t, -30, repo.balances[s.MerchantID], 1e-9)
	require.InDelta(t, 80, repo.frozen[s.MerchantID], 1e-9)
}

func TestDisputeAfterDeadlineIsRejected(t *testing.T) {
	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	ops, repo := newOpsTest(t, now)
	s := repo.seedSettlement(now.Add(-8*24*time.Hour), SettlementStatusConfirmed, 10)

	err := ops.Dispute(context.Background(), s.ID, "late")
	require.ErrorIs(t, err, ErrSettlementDisputeWindowClosed)
	require.Equal(t, SettlementStatusConfirmed, repo.settlements[s.ID].Status)
}

func TestExploreReturnsSettlementItems(t *testing.T) {
	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	ops, repo := newOpsTest(t, now)
	s := repo.seedSettlement(now.Add(-2*24*time.Hour), SettlementStatusPending, 10)
	repo.seedItem(s.ID, 101, 7, 0.56)
	repo.seedItem(s.ID, 102, 3, 0.24)

	items, err := ops.Explore(context.Background(), s.ID)
	require.NoError(t, err)
	require.Len(t, items, 2)
	require.Equal(t, int64(101), items[0].UsageLogID)
	require.InDelta(t, 7, items[0].Cost, 1e-9)
	require.InDelta(t, 0.56, items[0].Commission, 1e-9)

	_, err = ops.Explore(context.Background(), 999)
	require.ErrorIs(t, err, ErrSettlementNotFound)
}

func TestSettlementServiceRunOnceAlsoAutoConfirms(t *testing.T) {
	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	ops, repo := newOpsTest(t, now)
	s := repo.seedSettlement(now.Add(-8*24*time.Hour), SettlementStatusPending, 20)

	svc := newTestSettlementService(newFakeSettlementRepo(), now)
	svc.SetAutoConfirmer(ops)
	svc.runOnce()

	require.Equal(t, SettlementStatusConfirmed, repo.settlements[s.ID].Status)
	require.InDelta(t, 20, repo.balances[s.MerchantID], 1e-9)
}

func TestSettlementStatusMachineAllowsDispute(t *testing.T) {
	pending := &Settlement{Status: SettlementStatusPending}
	require.True(t, pending.CanAppeal())
	require.NoError(t, pending.TransitionTo(SettlementStatusDisputed))
	require.False(t, pending.CanAppeal())

	require.False(t, pending.CanTransitionTo(SettlementStatusPaid))
	require.NoError(t, pending.TransitionTo(SettlementStatusConfirmed))

	confirmed := &Settlement{Status: SettlementStatusConfirmed}
	require.NoError(t, confirmed.TransitionTo(SettlementStatusDisputed))

	paid := &Settlement{Status: SettlementStatusPaid}
	require.False(t, paid.CanTransitionTo(SettlementStatusDisputed))
	require.False(t, paid.CanTransitionTo(SettlementStatusConfirmed))
	if err := paid.TransitionTo(SettlementStatusConfirmed); !errors.Is(err, ErrSettlementStatusTransition) {
		t.Fatalf("paid is terminal, got %v", err)
	}
}
