package service

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeMerchantConsoleRepo struct {
	balance float64
	points  []MerchantDailyUsagePoint
	err     error

	gotMerchantID int64
	gotFrom       time.Time
	gotTo         time.Time
}

func (r *fakeMerchantConsoleRepo) PayableBalance(_ context.Context, merchantID int64) (float64, error) {
	r.gotMerchantID = merchantID
	return r.balance, r.err
}

func (r *fakeMerchantConsoleRepo) DailyUsage(
	_ context.Context,
	merchantID int64,
	from, to time.Time,
) ([]MerchantDailyUsagePoint, error) {
	r.gotMerchantID = merchantID
	r.gotFrom = from
	r.gotTo = to
	return r.points, r.err
}

func newMerchantConsoleTestService(t *testing.T, repo *fakeMerchantConsoleRepo) *MerchantConsoleService {
	t.Helper()

	merchantRepo := newFakeMerchantRepo()
	_, err := NewMerchantService(merchantRepo).Create(context.Background(), CreateMerchantInput{
		Name:           "上游一号",
		CommissionRate: merchantRatePtr(0.1),
	})
	if err != nil {
		t.Fatalf("create merchant: %v", err)
	}

	svc := NewMerchantConsoleService(NewMerchantService(merchantRepo), repo)
	svc.now = func() time.Time { return time.Date(2026, 10, 30, 15, 4, 5, 0, time.UTC) }
	return svc
}

func TestMerchantConsoleDashboardUsesAuthenticatedMerchant(t *testing.T) {
	repo := &fakeMerchantConsoleRepo{balance: 12.34}
	svc := newMerchantConsoleTestService(t, repo)

	got, err := svc.Dashboard(context.Background(), 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.MerchantID != 1 || got.Name != "上游一号" {
		t.Fatalf("unexpected dashboard: %+v", got)
	}
	if got.CommissionRate != 0.1 || got.PayableBalance != 12.34 {
		t.Fatalf("unexpected money: %+v", got)
	}
	if repo.gotMerchantID != 1 {
		t.Fatalf("want repo scoped to merchant 1, got %d", repo.gotMerchantID)
	}
}

func TestMerchantConsoleDashboardRejectsMissingMerchant(t *testing.T) {
	repo := &fakeMerchantConsoleRepo{}
	svc := newMerchantConsoleTestService(t, repo)

	if _, err := svc.Dashboard(context.Background(), 999); !errors.Is(err, ErrMerchantNotFound) {
		t.Fatalf("want ErrMerchantNotFound, got %v", err)
	}
}

func TestMerchantConsoleDailyUsageFillsMissingDays(t *testing.T) {
	repo := &fakeMerchantConsoleRepo{
		points: []MerchantDailyUsagePoint{
			{Date: "2026-10-30", Requests: 3, GrossCost: 3, Commission: 0.3, NetPayable: 2.7},
		},
	}
	svc := newMerchantConsoleTestService(t, repo)

	trend, err := svc.DailyUsage(context.Background(), 1, 7)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if trend.Days != 7 || trend.StartDate != "2026-10-24" || trend.EndDate != "2026-10-30" {
		t.Fatalf("unexpected window: %+v", trend)
	}
	if len(trend.Points) != 7 {
		t.Fatalf("want 7 points, got %d", len(trend.Points))
	}
	if trend.Points[0].Date != "2026-10-24" || trend.Points[0].Requests != 0 {
		t.Fatalf("want zero-filled first day, got %+v", trend.Points[0])
	}
	if trend.Points[6].Date != "2026-10-30" || trend.Points[6].NetPayable != 2.7 {
		t.Fatalf("want today's usage, got %+v", trend.Points[6])
	}

	wantFrom := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	wantTo := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	if !repo.gotFrom.Equal(wantFrom) || !repo.gotTo.Equal(wantTo) {
		t.Fatalf("want [%s, %s), got [%s, %s)", wantFrom, wantTo, repo.gotFrom, repo.gotTo)
	}
	if repo.gotMerchantID != 1 {
		t.Fatalf("want repo scoped to merchant 1, got %d", repo.gotMerchantID)
	}
}

func TestMerchantConsoleDailyUsageClampsWindow(t *testing.T) {
	svc := newMerchantConsoleTestService(t, &fakeMerchantConsoleRepo{})

	for _, tc := range []struct {
		in   int
		want int
	}{
		{in: 0, want: MerchantUsageTrendDefaultDays},
		{in: -3, want: MerchantUsageTrendDefaultDays},
		{in: MerchantUsageTrendMaxDays + 10, want: MerchantUsageTrendMaxDays},
	} {
		trend, err := svc.DailyUsage(context.Background(), 1, tc.in)
		if err != nil {
			t.Fatalf("days %d: unexpected error: %v", tc.in, err)
		}
		if trend.Days != tc.want || len(trend.Points) != tc.want {
			t.Fatalf("days %d: want %d, got days=%d points=%d", tc.in, tc.want, trend.Days, len(trend.Points))
		}
	}
}
