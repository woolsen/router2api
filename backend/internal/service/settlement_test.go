package service

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"
)

// fakeSettlementUsage is one usage_logs row seeded into the fake repository.
// The fake deliberately does not re-implement the SQL filters: production
// filtering lives in repository.AggregateUsage (billing_type, actual_cost,
// commission_rate IS NOT NULL) and is covered by the repository SQL test.
type fakeSettlementUsage struct {
	cost float64
	rate float64
}

type fakeSettlementRepo struct {
	usages      map[int64][]fakeSettlementUsage
	settlements map[string]*Settlement
	nextID      int64

	lastMerchantID int64
	lastFrom       time.Time
	lastTo         time.Time
	aggregateCalls int
}

func newFakeSettlementRepo() *fakeSettlementRepo {
	return &fakeSettlementRepo{
		usages:      make(map[int64][]fakeSettlementUsage),
		settlements: make(map[string]*Settlement),
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
	r.aggregateCalls++

	var agg SettlementUsageAggregate
	for _, u := range r.usages[merchantID] {
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
	return ids, nil
}

func TestGenerateSettlementAggregatesUsage(t *testing.T) {
	ctx := context.Background()
	repo := newFakeSettlementRepo()
	repo.usages[1] = []fakeSettlementUsage{
		{cost: 10, rate: 0.08},
		{cost: 20, rate: 0.08},
		{cost: 5, rate: 0.08},
	}
	svc := NewSettlementService(repo)

	from := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)
	to := from.Add(7 * 24 * time.Hour)

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
	repo.usages[1] = []fakeSettlementUsage{{cost: 10, rate: 0.08}}
	svc := NewSettlementService(repo)

	from := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)
	to := from.Add(7 * 24 * time.Hour)

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

func TestGenerateSettlementLeavesLateWriteBuffer(t *testing.T) {
	ctx := context.Background()
	repo := newFakeSettlementRepo()
	svc := NewSettlementService(repo)

	// 带非 UTC 时区与亚秒偏移，验证存储/聚合窗口统一落到 UTC。
	loc := time.FixedZone("CST", 8*3600)
	from := time.Date(2026, 3, 2, 8, 0, 0, 0, loc)
	to := time.Date(2026, 3, 9, 8, 0, 0, 0, loc)

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
	// usage_logs 异步落库：右侧必须留 24h 缓冲，且不结 period_end 之后的迟到行。
	if got, want := repo.lastTo, to.UTC().Add(-24*time.Hour); !got.Equal(want) {
		t.Fatalf("aggregate to want %v (period_end-24h), got %v", want, got)
	}
}

func TestGenerateSettlementRejectsInvalidPeriod(t *testing.T) {
	svc := NewSettlementService(newFakeSettlementRepo())
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
	svc := NewSettlementService(repo)

	from := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)
	s, err := svc.Generate(ctx, 42, from, from.Add(7*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if s.GrossCost != 0 || s.Commission != 0 || s.NetPayable != 0 {
		t.Fatalf("empty period must settle to zero, got %+v", s)
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

func TestSettlementServiceRunOnceGeneratesPreviousWeekOnce(t *testing.T) {
	repo := newFakeSettlementRepo()
	repo.usages[1] = []fakeSettlementUsage{{cost: 100, rate: 0.08}}
	svc := NewSettlementService(repo)
	// 2026-03-11 是周三，上一自然周为 03-02 .. 03-09（UTC）。
	svc.now = func() time.Time { return time.Date(2026, 3, 11, 12, 0, 0, 0, time.UTC) }

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

	// 同一账期每小时 tick 一次不应重复聚合/生成。
	calls := repo.aggregateCalls
	svc.runOnce()
	if repo.aggregateCalls != calls {
		t.Fatalf("same period must not aggregate again, calls %d -> %d", calls, repo.aggregateCalls)
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
