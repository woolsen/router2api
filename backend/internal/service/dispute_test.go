package service

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type fakeDisputeRepository struct {
	disputes []Dispute

	openSettlementCalls int
	openUsageCalls      int
	resolveCalls        int
	adjustCalls         int

	lastOpen    OpenDisputeInput
	lastResolve ResolveInput
	lastAdjust  struct {
		merchantID int64
		amount     float64
		refType    string
		refID      int64
	}
}

func (f *fakeDisputeRepository) OpenSettlement(_ context.Context, in OpenDisputeInput, _ time.Time) (*Dispute, error) {
	f.openSettlementCalls++
	f.lastOpen = in
	return &Dispute{ID: 1, OwnerType: in.OwnerType, OwnerID: in.OwnerID,
		TargetType: in.TargetType, TargetID: in.TargetID, Amount: in.Amount,
		Reason: in.Reason, Status: DisputeStatusOpen}, nil
}

func (f *fakeDisputeRepository) OpenUsage(_ context.Context, in OpenDisputeInput, _ time.Time) (*Dispute, error) {
	f.openUsageCalls++
	f.lastOpen = in
	return &Dispute{ID: 2, OwnerType: in.OwnerType, OwnerID: in.OwnerID,
		TargetType: in.TargetType, TargetID: in.TargetID, Amount: in.Amount,
		Reason: in.Reason, Status: DisputeStatusOpen}, nil
}

func (f *fakeDisputeRepository) ListByMerchant(context.Context, int64) ([]Dispute, error) {
	return f.disputes, nil
}

func (f *fakeDisputeRepository) ListByUser(context.Context, int64) ([]Dispute, error) {
	return f.disputes, nil
}

func (f *fakeDisputeRepository) ListAll(context.Context, string) ([]Dispute, error) {
	return f.disputes, nil
}

func (f *fakeDisputeRepository) Resolve(_ context.Context, id int64, in ResolveInput, _ time.Time) (*Dispute, error) {
	f.resolveCalls++
	f.lastResolve = in
	return &Dispute{ID: id, Status: DisputeStatusResolved, Decision: in.Decision,
		AdjustAmount: in.AdjustAmount, Note: in.Note, ResolvedBy: in.By}, nil
}

func (f *fakeDisputeRepository) AdjustLedger(
	_ context.Context,
	merchantID int64,
	amount float64,
	refType string,
	refID int64,
	_ time.Time,
) error {
	f.adjustCalls++
	f.lastAdjust.merchantID = merchantID
	f.lastAdjust.amount = amount
	f.lastAdjust.refType = refType
	f.lastAdjust.refID = refID
	return nil
}

func newDisputeTestService() (*DisputeService, *fakeDisputeRepository) {
	repo := &fakeDisputeRepository{}
	svc := NewDisputeService(repo)
	svc.now = func() time.Time { return time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC) }
	return svc, repo
}

func TestDisputeOpenValidatesInput(t *testing.T) {
	svc, repo := newDisputeTestService()
	ctx := context.Background()

	cases := []struct {
		name string
		in   OpenDisputeInput
		want error
	}{
		{
			name: "empty reason",
			in: OpenDisputeInput{OwnerType: DisputeOwnerMerchant, OwnerID: 1,
				TargetType: DisputeTargetSettlement, TargetID: 9, Amount: 1},
			want: ErrDisputeReasonRequired,
		},
		{
			name: "negative amount",
			in: OpenDisputeInput{OwnerType: DisputeOwnerMerchant, OwnerID: 1,
				TargetType: DisputeTargetSettlement, TargetID: 9, Amount: -1, Reason: "x"},
			want: ErrDisputeAmountInvalid,
		},
		{
			name: "nan amount",
			in: OpenDisputeInput{OwnerType: DisputeOwnerMerchant, OwnerID: 1,
				TargetType: DisputeTargetSettlement, TargetID: 9, Amount: math.NaN(), Reason: "x"},
			want: ErrDisputeAmountInvalid,
		},
		{
			name: "user cannot target settlement",
			in: OpenDisputeInput{OwnerType: DisputeOwnerUser, OwnerID: 1,
				TargetType: DisputeTargetSettlement, TargetID: 9, Amount: 1, Reason: "x"},
			want: ErrDisputeTargetTypeInvalid,
		},
		{
			name: "merchant cannot target usage",
			in: OpenDisputeInput{OwnerType: DisputeOwnerMerchant, OwnerID: 1,
				TargetType: DisputeTargetUsage, TargetID: 9, Amount: 1, Reason: "x"},
			want: ErrDisputeTargetTypeInvalid,
		},
		{
			name: "unknown target",
			in: OpenDisputeInput{OwnerType: DisputeOwnerMerchant, OwnerID: 1,
				TargetType: "withdrawal", TargetID: 9, Amount: 1, Reason: "x"},
			want: ErrDisputeTargetTypeInvalid,
		},
		{
			name: "missing target id",
			in: OpenDisputeInput{OwnerType: DisputeOwnerMerchant, OwnerID: 1,
				TargetType: DisputeTargetSettlement, TargetID: 0, Amount: 1, Reason: "x"},
			want: ErrDisputeTargetTypeInvalid,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.Open(ctx, tc.in)
			require.ErrorIs(t, err, tc.want)
		})
	}
	require.Zero(t, repo.openSettlementCalls+repo.openUsageCalls)
}

func TestDisputeOpenDelegatesByTargetType(t *testing.T) {
	svc, repo := newDisputeTestService()
	ctx := context.Background()

	d, err := svc.Open(ctx, OpenDisputeInput{
		OwnerType: DisputeOwnerMerchant, OwnerID: 7,
		TargetType: DisputeTargetSettlement, TargetID: 9,
		Amount: 3, Reason: "  用量偏高  ",
	})
	require.NoError(t, err)
	require.Equal(t, int64(1), d.ID)
	require.Equal(t, 1, repo.openSettlementCalls)
	require.Equal(t, "用量偏高", repo.lastOpen.Reason)

	d, err = svc.Open(ctx, OpenDisputeInput{
		OwnerType: DisputeOwnerUser, OwnerID: 42,
		TargetType: DisputeTargetUsage, TargetID: 99,
		Amount: 0.5, Reason: "wrong charge",
	})
	require.NoError(t, err)
	require.Equal(t, int64(2), d.ID)
	require.Equal(t, 1, repo.openUsageCalls)
}

func TestDisputeResolveNormalizesAndDelegates(t *testing.T) {
	svc, repo := newDisputeTestService()

	d, err := svc.Resolve(context.Background(), 5, ResolveInput{
		Decision: " ADJUST ", AdjustAmount: -3.12345678901, Note: "  核对误差 ", By: "  ",
	})
	require.NoError(t, err)
	require.Equal(t, DisputeStatusResolved, d.Status)
	require.Equal(t, DisputeDecisionAdjust, repo.lastResolve.Decision)
	require.InDelta(t, -3.123456789, repo.lastResolve.AdjustAmount, 1e-12)
	require.Equal(t, "核对误差", repo.lastResolve.Note)
	require.Equal(t, DisputeDefaultOperator, repo.lastResolve.By)
}

func TestDisputeResolveRejectsUnknownDecision(t *testing.T) {
	svc, repo := newDisputeTestService()

	_, err := svc.Resolve(context.Background(), 5, ResolveInput{Decision: "approve"})
	require.ErrorIs(t, err, ErrDisputeDecisionInvalid)
	require.Zero(t, repo.resolveCalls)
}

func TestDisputeResolveIgnoresAdjustAmountForReject(t *testing.T) {
	svc, repo := newDisputeTestService()

	_, err := svc.Resolve(context.Background(), 5, ResolveInput{
		Decision: DisputeDecisionReject, AdjustAmount: 99, By: "admin",
	})
	require.NoError(t, err)
	require.Zero(t, repo.lastResolve.AdjustAmount)
}

func TestDisputeAdjustLedgerRequiresDisputeRef(t *testing.T) {
	svc, repo := newDisputeTestService()

	require.ErrorIs(t, svc.AdjustLedger(context.Background(), 1, -3, "", 0), ErrAdjustRequiresSource)
	require.ErrorIs(t, svc.AdjustLedger(context.Background(), 1, -3, "dispute", 0), ErrAdjustRequiresSource)
	require.ErrorIs(t, svc.AdjustLedger(context.Background(), 1, -3, "settlement", 9), ErrAdjustRequiresSource)
	require.Zero(t, repo.adjustCalls)

	require.NoError(t, svc.AdjustLedger(context.Background(), 1, -3, "dispute", 9))
	require.Equal(t, 1, repo.adjustCalls)
	require.Equal(t, "dispute", repo.lastAdjust.refType)
	require.Equal(t, int64(9), repo.lastAdjust.refID)
}

func TestDisputeListAllRejectsUnknownStatus(t *testing.T) {
	svc, _ := newDisputeTestService()

	_, err := svc.ListAll(context.Background(), "paid")
	require.ErrorIs(t, err, ErrDisputeStatusInvalid)

	_, err = svc.ListAll(context.Background(), "")
	require.NoError(t, err)
}

func TestDisputeListByMerchantRejectsInvalidID(t *testing.T) {
	svc, _ := newDisputeTestService()

	_, err := svc.ListByMerchant(context.Background(), 0)
	require.ErrorIs(t, err, ErrDisputeTargetNotFound)
}
