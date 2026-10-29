package service

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"testing"
	"time"

	"github.com/woolsen/router2api/internal/config"
	"github.com/stretchr/testify/require"
)

// fakeSettlementUsage is one usage_logs row seeded into the fake repository.
// The fake deliberately does not re-implement the SQL filters: production
// filtering lives in repository.AggregateUsage (billing_type, actual_cost,
// commission_rate IS NOT NULL) and is covered by the repository SQL test.
type fakeSettlementUsage struct {
	cost float64
	rate float64
	at   time.Time
}

type fakeSettlementRepo struct {
	usages      map[int64][]fakeSettlementUsage
	settlements map[string]*Settlement
	nextID      int64

	aggregateErrByMerchant   map[int64]error
	aggregateCallsByMerchant map[int64]int
	unknownCommissionRows    map[int64]int64
	lastMerchantID           int64
	lastFrom                 time.Time
	lastTo                   time.Time
}

func newFakeSettlementRepo() *fakeSettlementRepo {
	return &fakeSettlementRepo{
		usages:                   make(map[int64][]fakeSettlementUsage),
		settlements:              make(map[string]*Settlement),
		aggregateErrByMerchant:   make(map[int64]error),
		aggregateCallsByMerchant: make(map[int64]int),
		unknownCommissionRows:    make(map[int64]int64),
	}
}

func (r *fakeSettlementRepo) AggregateUsage(
	_ context.Context,
	merchantID int64,
	from, to time.Time,
) (SettlementUsageAggregate, error) {
	r.lastMerchantID = merchantID
	r.lastFrom = from
	r.lastTo = to
	r.aggregateCallsByMerchant[merchantID]++
	if err := r.aggregateErrByMerchant[merchantID]; err != nil {
		return SettlementUsageAggregate{}, err
	}

	var agg SettlementUsageAggregate
	agg.ExcludedUnknownCommissionRows = r.unknownCommissionRows[merchantID]
	for _, u := range r.usages[merchantID] {
		if !u.at.IsZero() && (u.at.Before(from) || !u.at.Before(to)) {
			continue
		}
		agg.GrossCost += u.cost
		agg.Commission += u.cost * u.rate
	}
	return agg, nil
}

func (r *fakeSettlementRepo) CreateOrGet(_ context.Context, s *Settlement) (*Settlement, error) {
	key := fmt.Sprintf("%d|%d|%d", s.MerchantID, s.PeriodStart.UnixNano(), s.PeriodEnd.UnixNano())
	if existing, ok := r.settlements[key]; ok {
		return existing, nil
	}
	r.nextID++
	cp := *s
	cp.ID = r.nextID
	r.settlements[key] = &cp
	return &cp, nil
}

func (r *fakeSettlementRepo) ListActiveMerchantIDs(_ context.Context) ([]int64, error) {
	ids := make([]int64, 0, len(r.usages))
	for id := range r.usages {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, nil
}

func (r *fakeSettlementRepo) EarliestSettleableUsageAt(_ context.Context, merchantID int64) (*time.Time, error) {
	var earliest *time.Time
	for _, u := range r.usages[merchantID] {
		if u.at.IsZero() {
			continue
		}
		at := u.at
		if earliest == nil || at.Before(*earliest) {
			earliest = &at
		}
	}
	return earliest, nil
}

func (r *fakeSettlementRepo) ListSettledPeriodStarts(_ context.Context, merchantID int64) ([]time.Time, error) {
	seen := make(map[int64]struct{})
	var starts []time.Time
	for _, s := range r.settlements {
		if s.MerchantID != merchantID {
			continue
		}
		start := s.PeriodStart.UTC()
		if _, ok := seen[start.Unix()]; ok {
			continue
		}
		seen[start.Unix()] = struct{}{}
		starts = append(starts, start)
	}
	return starts, nil
}

func newTestSettlementService(repo SettlementRepository, now time.Time) *SettlementService {
	svc := NewSettlementService(repo, nil)
	svc.now = func() time.Time { return now }
	return svc
}

func TestGenerateSettlementAggregatesUsage(t *testing.T) {
	ctx := context.Background()
	repo := newFakeSettlementRepo()
	from := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)
	to := from.Add(7 * 24 * time.Hour)
	repo.usages[1] = []fakeSettlementUsage{
		{cost: 10, rate: 0.08, at: from.Add(1 * time.Hour)},
		{cost: 20, rate: 0.08, at: from.Add(2 * time.Hour)},
		{cost: 5, rate: 0.08, at: from.Add(3 * time.Hour)},
	}
	svc := newTestSettlementService(repo, to.Add(SettlementCloseDelay+time.Hour))

	s, err := svc.Generate(ctx, 1, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(s.GrossCost-35) > 1e-9 {
		t.Fatalf("gross want 35, got %v", s.GrossCost)
	}
	if math.Abs(s.Commission-2.8) > 1e-9 {
		t.Fatalf("commission want 2.8, got %v", s.Commission)
	}
	if math.Abs(s.NetPayable-32.2) > 1e-9 {
		t.Fatalf("net want 32.2, got %v", s.NetPayable)
	}
	if s.Status != SettlementStatusPending {
		t.Fatalf("want pending, got %s", s.Status)
	}
}

func TestGenerateSettlementIsIdempotentPerPeriod(t *testing.T) {
	ctx := context.Background()
	repo := newFakeSettlementRepo()
	from := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)
	to := from.Add(7 * 24 * time.Hour)
	repo.usages[1] = []fakeSettlementUsage{{cost: 10, rate: 0.08, at: from}}
	svc := newTestSettlementService(repo, to.Add(SettlementCloseDelay+time.Hour))

	first, err := svc.Generate(ctx, 1, from, to)
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.Generate(ctx, 1, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("same merchant+period must return the same settlement, got %d and %d", first.ID, second.ID)
	}
}

func TestGenerateSettlementRefusesUnknownCommissionRows(t *testing.T) {
	ctx := context.Background()
	repo := newFakeSettlementRepo()
	from := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)
	to := from.Add(7 * 24 * time.Hour)
	repo.usages[1] = []fakeSettlementUsage{{cost: 10, rate: 0.08, at: from.Add(time.Hour)}}
	repo.unknownCommissionRows[1] = 2
	svc := newTestSettlementService(repo, to.Add(SettlementCloseDelay+time.Hour))

	got, err := svc.Generate(ctx, 1, from, to)
	require.ErrorIs(t, err, ErrSettlementUnknownCommission)
	require.Nil(t, got, "must not create a settlement while commission is unknown")
	require.Empty(t, repo.settlements, "no settlement may be persisted before rate backfill")

	// 回填后（NULL 行数归零）重跑同一账期必须正常落单，金额覆盖全部用量。
	repo.unknownCommissionRows[1] = 0
	s, err := svc.Generate(ctx, 1, from, to)
	require.NoError(t, err)
	require.NotNil(t, s)
	require.InDelta(t, 10, s.GrossCost, 1e-9)
	require.InDelta(t, 0.8, s.Commission, 1e-9)
}

func TestGenerateSettlementSettlesFullPeriod(t *testing.T) {
	ctx := context.Background()
	repo := newFakeSettlementRepo()

	// 带非 UTC 时区，验证存储/聚合窗口统一落到 UTC。
	loc := time.FixedZone("CST", 8*3600)
	from := time.Date(2026, 3, 2, 8, 0, 0, 0, loc)
	to := time.Date(2026, 3, 9, 8, 0, 0, 0, loc)
	// 账期最后 1 小时的用量也必须结算（右侧不做 24h 截断）。
	repo.usages[7] = []fakeSettlementUsage{
		{cost: 10, rate: 0.08, at: from.Add(1 * time.Hour)},
		{cost: 5, rate: 0.08, at: to.Add(-1 * time.Hour)},
	}
	svc := newTestSettlementService(repo, to.Add(SettlementCloseDelay+time.Hour))

	s, err := svc.Generate(ctx, 7, from, to)
	if err != nil {
		t.Fatal(err)
	}

	if got, want := s.PeriodStart, from.UTC(); !got.Equal(want) || got.Location() != time.UTC {
		t.Fatalf("period start want %v (UTC), got %v (%v)", want, got, got.Location())
	}
	if got, want := s.PeriodEnd, to.UTC(); !got.Equal(want) || got.Location() != time.UTC {
		t.Fatalf("period end want %v (UTC), got %v (%v)", want, got, got.Location())
	}
	if got, want := repo.lastFrom, from.UTC(); !got.Equal(want) {
		t.Fatalf("aggregate from want %v, got %v", want, got)
	}
	if got, want := repo.lastTo, to.UTC(); !got.Equal(want) {
		t.Fatalf("aggregate window must be the full period: want %v, got %v", want, got)
	}
	if math.Abs(s.GrossCost-15) > 1e-9 {
		t.Fatalf("gross want 15 (including last hour), got %v", s.GrossCost)
	}
}

func TestGenerateSettlementRejectsOpenPeriod(t *testing.T) {
	ctx := context.Background()
	repo := newFakeSettlementRepo()
	from := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)
	to := from.Add(7 * 24 * time.Hour)

	// period_end + 24h 之前不允许封账。
	svc := newTestSettlementService(repo, to.Add(SettlementCloseDelay-time.Minute))
	if _, err := svc.Generate(ctx, 1, from, to); !errors.Is(err, ErrSettlementPeriodNotClosed) {
		t.Fatalf("want ErrSettlementPeriodNotClosed, got %v", err)
	}

	// 到达 period_end + 24h 即可生成。
	svc = newTestSettlementService(repo, to.Add(SettlementCloseDelay))
	if _, err := svc.Generate(ctx, 1, from, to); err != nil {
		t.Fatalf("period must be settleable at period_end+24h: %v", err)
	}
}

func TestGenerateSettlementRejectsInvalidPeriod(t *testing.T) {
	svc := newTestSettlementService(newFakeSettlementRepo(), time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC))
	from := time.Date(2026, 3, 9, 0, 0, 0, 0, time.UTC)

	if _, err := svc.Generate(context.Background(), 1, from, from); err == nil {
		t.Fatal("zero-length period must be rejected")
	}
	if _, err := svc.Generate(context.Background(), 0, from, from.Add(time.Hour)); err == nil {
		t.Fatal("merchant id 0 must be rejected")
	}
}

func TestGenerateSettlementHandlesEmptyPeriod(t *testing.T) {
	ctx := context.Background()
	repo := newFakeSettlementRepo()
	from := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)
	to := from.Add(7 * 24 * time.Hour)
	svc := newTestSettlementService(repo, to.Add(SettlementCloseDelay))

	s, err := svc.Generate(ctx, 42, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if s.GrossCost != 0 || s.Commission != 0 || s.NetPayable != 0 {
		t.Fatalf("empty period must settle to zero, got %+v", s)
	}
}

func TestGenerateSettlementRefusedInSimpleMode(t *testing.T) {
	ctx := context.Background()
	repo := newFakeSettlementRepo()
	from := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)
	to := from.Add(7 * 24 * time.Hour)
	repo.usages[1] = []fakeSettlementUsage{{cost: 10, rate: 0.08, at: from}}

	svc := NewSettlementService(repo, &config.Config{RunMode: config.RunModeSimple})
	svc.now = func() time.Time { return to.Add(SettlementCloseDelay + time.Hour) }

	if _, err := svc.Generate(ctx, 1, from, to); !errors.Is(err, ErrSettlementSimpleMode) {
		t.Fatalf("want ErrSettlementSimpleMode, got %v", err)
	}
	svc.runOnce()
	if len(repo.settlements) != 0 {
		t.Fatalf("simple mode must not generate settlements, got %d", len(repo.settlements))
	}
}

func TestSettlementServiceRunOnceGeneratesPreviousWeekOnce(t *testing.T) {
	repo := newFakeSettlementRepo()
	repo.usages[1] = []fakeSettlementUsage{
		{cost: 100, rate: 0.08, at: time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC)},
	}
	// 2026-03-11 是周三，上一自然周为 03-02 .. 03-09（UTC），已过 +24h 封账期。
	svc := newTestSettlementService(repo, time.Date(2026, 3, 11, 12, 0, 0, 0, time.UTC))

	svc.runOnce()
	if len(repo.settlements) != 1 {
		t.Fatalf("want 1 settlement, got %d", len(repo.settlements))
	}
	for _, s := range repo.settlements {
		if want := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC); !s.PeriodStart.Equal(want) {
			t.Fatalf("period start want %v, got %v", want, s.PeriodStart)
		}
		if want := time.Date(2026, 3, 9, 0, 0, 0, 0, time.UTC); !s.PeriodEnd.Equal(want) {
			t.Fatalf("period end want %v, got %v", want, s.PeriodEnd)
		}
		if math.Abs(s.NetPayable-92) > 1e-9 {
			t.Fatalf("net want 92, got %v", s.NetPayable)
		}
	}

	// 已结算的账期不应重复聚合/生成。
	calls := repo.aggregateCallsByMerchant[1]
	svc.runOnce()
	if repo.aggregateCallsByMerchant[1] != calls {
		t.Fatalf("settled period must not aggregate again, calls %d -> %d", calls, repo.aggregateCallsByMerchant[1])
	}
}

func TestSettlementServiceWaitsForCloseDelay(t *testing.T) {
	repo := newFakeSettlementRepo()
	repo.usages[1] = []fakeSettlementUsage{
		{cost: 100, rate: 0.08, at: time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC)},
	}

	// 周一 23:00：period_end (03-09 00:00) + 24h 尚未到达，不能生成。
	svc := newTestSettlementService(repo, time.Date(2026, 3, 9, 23, 0, 0, 0, time.UTC))
	svc.runOnce()
	if len(repo.settlements) != 0 {
		t.Fatalf("must not settle before period_end+24h, got %d settlements", len(repo.settlements))
	}

	// 周二 00:00 之后的第一次 tick 生成刚结束的一周。
	svc = newTestSettlementService(repo, time.Date(2026, 3, 10, 0, 30, 0, 0, time.UTC))
	svc.runOnce()
	if len(repo.settlements) != 1 {
		t.Fatalf("want 1 settlement after close delay, got %d", len(repo.settlements))
	}
}

func TestSettlementServiceBackfillsMissingWeeks(t *testing.T) {
	repo := newFakeSettlementRepo()
	repo.usages[1] = []fakeSettlementUsage{
		{cost: 100, rate: 0.08, at: time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC)}, // week 03-02..03-09
		{cost: 50, rate: 0.08, at: time.Date(2026, 3, 18, 12, 0, 0, 0, time.UTC)}, // week 03-16..03-23
	}
	// 2026-03-25 周三：可封账到 03-23，中间断掉的一周也要补出来。
	svc := newTestSettlementService(repo, time.Date(2026, 3, 25, 12, 0, 0, 0, time.UTC))

	svc.runOnce()

	if len(repo.settlements) != 3 {
		t.Fatalf("want 3 weekly settlements (including the missing week), got %d", len(repo.settlements))
	}
	byStart := make(map[string]*Settlement)
	for _, s := range repo.settlements {
		byStart[s.PeriodStart.Format("2006-01-02")] = s
	}
	want := map[string]float64{
		"2026-03-02": 100,
		"2026-03-09": 0,
		"2026-03-16": 50,
	}
	for start, gross := range want {
		s, ok := byStart[start]
		if !ok {
			t.Fatalf("missing settlement for week starting %s", start)
		}
		if math.Abs(s.GrossCost-gross) > 1e-9 {
			t.Fatalf("week %s gross want %v, got %v", start, gross, s.GrossCost)
		}
	}
}

func TestSettlementServiceRetriesOnlyFailedMerchant(t *testing.T) {
	repo := newFakeSettlementRepo()
	at := time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC)
	repo.usages[1] = []fakeSettlementUsage{{cost: 10, rate: 0.08, at: at}}
	repo.usages[2] = []fakeSettlementUsage{{cost: 20, rate: 0.08, at: at}}
	repo.aggregateErrByMerchant[2] = errors.New("db down")

	svc := newTestSettlementService(repo, time.Date(2026, 3, 11, 12, 0, 0, 0, time.UTC))

	svc.runOnce()
	if len(repo.settlements) != 1 {
		t.Fatalf("merchant 1 must settle despite merchant 2 failure, got %d", len(repo.settlements))
	}
	if got := repo.aggregateCallsByMerchant[1]; got != 1 {
		t.Fatalf("merchant 1 aggregate calls want 1, got %d", got)
	}

	delete(repo.aggregateErrByMerchant, 2)
	svc.runOnce()
	if len(repo.settlements) != 2 {
		t.Fatalf("merchant 2 must be retried on the next tick, got %d settlements", len(repo.settlements))
	}
	if got := repo.aggregateCallsByMerchant[1]; got != 1 {
		t.Fatalf("failed merchant retry must not re-run merchant 1, calls %d", got)
	}
}

func TestSettlementStatusMachine(t *testing.T) {
	s := &Settlement{Status: SettlementStatusPending}

	if err := s.TransitionTo(SettlementStatusPaid); err == nil {
		t.Fatal("pending must not jump straight to paid")
	}
	if err := s.TransitionTo(SettlementStatusConfirmed); err != nil {
		t.Fatalf("pending -> confirmed: %v", err)
	}
	if err := s.TransitionTo(SettlementStatusPaid); err != nil {
		t.Fatalf("confirmed -> paid: %v", err)
	}
	if err := s.TransitionTo(SettlementStatusConfirmed); err == nil {
		t.Fatal("paid is terminal")
	}
}

func TestStartOfUTCWeek(t *testing.T) {
	cases := []struct {
		in   time.Time
		want time.Time
	}{
		{time.Date(2026, 3, 9, 0, 0, 0, 0, time.UTC), time.Date(2026, 3, 9, 0, 0, 0, 0, time.UTC)},
		{time.Date(2026, 3, 9, 23, 59, 59, 0, time.UTC), time.Date(2026, 3, 9, 0, 0, 0, 0, time.UTC)},
		{time.Date(2026, 3, 15, 23, 0, 0, 0, time.UTC), time.Date(2026, 3, 9, 0, 0, 0, 0, time.UTC)},
		// 周日 23:00 CST == 周日 15:00 UTC，仍属于上一个周一开头的自然周。
		{time.Date(2026, 3, 15, 23, 0, 0, 0, time.FixedZone("CST", 8*3600)), time.Date(2026, 3, 9, 0, 0, 0, 0, time.UTC)},
	}
	for _, tc := range cases {
		if got := startOfUTCWeek(tc.in); !got.Equal(tc.want) {
			t.Fatalf("startOfUTCWeek(%v) want %v, got %v", tc.in, tc.want, got)
		}
	}
}

func TestLatestSettleablePeriodEnd(t *testing.T) {
	cases := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{
			name: "周一封账期未过，最后可结的是上一周",
			now:  time.Date(2026, 3, 9, 23, 0, 0, 0, time.UTC),
			want: time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "周二 00:00 起可结刚结束的一周",
			now:  time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC),
			want: time.Date(2026, 3, 9, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "周日可结刚结束的一周",
			now:  time.Date(2026, 3, 15, 23, 0, 0, 0, time.UTC),
			want: time.Date(2026, 3, 9, 0, 0, 0, 0, time.UTC),
		},
	}
	for _, tc := range cases {
		if got := latestSettleablePeriodEnd(tc.now); !got.Equal(tc.want) {
			t.Fatalf("%s: want %v, got %v", tc.name, tc.want, got)
		}
	}
}

func TestSettlementAppealWindow(t *testing.T) {
	cases := []struct {
		status string
		want   bool
	}{
		{SettlementStatusPending, true},
		{SettlementStatusConfirmed, true},
		{SettlementStatusPaid, false},
		{"unknown", false},
	}
	for _, tc := range cases {
		s := &Settlement{Status: tc.status}
		if got := s.CanAppeal(); got != tc.want {
			t.Fatalf("status %q: CanAppeal want %v, got %v", tc.status, tc.want, got)
		}
	}
}
