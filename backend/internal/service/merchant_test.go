package service

import (
	"context"
	"errors"
	"testing"

	"github.com/woolsen/router2api/internal/pkg/pagination"
)

// fakeMerchantRepo is an in-memory MerchantRepository used by the service unit tests.
type fakeMerchantRepo struct {
	nextID int64
	items  map[int64]*Merchant
}

func newFakeMerchantRepo() *fakeMerchantRepo {
	return &fakeMerchantRepo{
		nextID: 1,
		items:  make(map[int64]*Merchant),
	}
}

func (r *fakeMerchantRepo) Create(_ context.Context, m *Merchant) error {
	m.ID = r.nextID
	r.nextID++
	cp := *m
	r.items[m.ID] = &cp
	return nil
}

func (r *fakeMerchantRepo) Get(_ context.Context, id int64) (*Merchant, error) {
	m, ok := r.items[id]
	if !ok {
		return nil, ErrMerchantNotFound
	}
	cp := *m
	return &cp, nil
}

func (r *fakeMerchantRepo) List(_ context.Context, params pagination.PaginationParams) ([]Merchant, *pagination.PaginationResult, error) {
	out := make([]Merchant, 0, len(r.items))
	for _, m := range r.items {
		out = append(out, *m)
	}
	return out, &pagination.PaginationResult{
		Total:    int64(len(out)),
		Page:     params.Page,
		PageSize: params.Limit(),
	}, nil
}

func (r *fakeMerchantRepo) UpdateCommissionRate(_ context.Context, id int64, rate float64) error {
	m, ok := r.items[id]
	if !ok {
		return ErrMerchantNotFound
	}
	m.CommissionRate = rate
	return nil
}

func merchantRatePtr(v float64) *float64 { return &v }

func TestMerchantServiceCreateDefaultsCommission(t *testing.T) {
	svc := NewMerchantService(newFakeMerchantRepo())
	m, err := svc.Create(context.Background(), CreateMerchantInput{Name: "上游一号", Contact: "tg:@one"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.CommissionRate != 0.08 {
		t.Fatalf("want 0.08, got %v", m.CommissionRate)
	}
	if m.Status != MerchantStatusActive {
		t.Fatalf("want active, got %s", m.Status)
	}
	if m.ID == 0 {
		t.Fatal("want repository-assigned ID, got 0")
	}
}

func TestMerchantServiceRejectsCommissionOutOfRange(t *testing.T) {
	svc := NewMerchantService(newFakeMerchantRepo())
	_, err := svc.Create(context.Background(), CreateMerchantInput{Name: "x", CommissionRate: merchantRatePtr(1.5)})
	if !errors.Is(err, ErrCommissionOutOfRange) {
		t.Fatalf("want ErrCommissionOutOfRange, got %v", err)
	}
}

func TestMerchantServiceAcceptsCommissionBounds(t *testing.T) {
	for _, rate := range []float64{0, 0.08, 0.999} {
		rate := rate
		svc := NewMerchantService(newFakeMerchantRepo())
		m, err := svc.Create(context.Background(), CreateMerchantInput{Name: "上游", CommissionRate: &rate})
		if err != nil {
			t.Fatalf("rate %v: unexpected error: %v", rate, err)
		}
		if m.CommissionRate != rate {
			t.Fatalf("rate %v: want %v, got %v", rate, rate, m.CommissionRate)
		}
	}
}

func TestMerchantServiceRejectsNegativeCommission(t *testing.T) {
	svc := NewMerchantService(newFakeMerchantRepo())
	_, err := svc.Create(context.Background(), CreateMerchantInput{Name: "x", CommissionRate: merchantRatePtr(-0.01)})
	if !errors.Is(err, ErrCommissionOutOfRange) {
		t.Fatalf("want ErrCommissionOutOfRange, got %v", err)
	}
}

func TestMerchantServiceCreateRejectsEmptyName(t *testing.T) {
	svc := NewMerchantService(newFakeMerchantRepo())
	_, err := svc.Create(context.Background(), CreateMerchantInput{Name: "   "})
	if !errors.Is(err, ErrMerchantNameRequired) {
		t.Fatalf("want ErrMerchantNameRequired, got %v", err)
	}
}

func TestMerchantServiceGetReturnsNotFound(t *testing.T) {
	svc := NewMerchantService(newFakeMerchantRepo())
	_, err := svc.Get(context.Background(), 42)
	if !errors.Is(err, ErrMerchantNotFound) {
		t.Fatalf("want ErrMerchantNotFound, got %v", err)
	}
}

func TestMerchantServiceSetCommissionRate(t *testing.T) {
	svc := NewMerchantService(newFakeMerchantRepo())
	created, err := svc.Create(context.Background(), CreateMerchantInput{Name: "上游"})
	if err != nil {
		t.Fatalf("unexpected create error: %v", err)
	}

	if err := svc.SetCommissionRate(context.Background(), created.ID, 0.12); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, err := svc.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("unexpected get error: %v", err)
	}
	if got.CommissionRate != 0.12 {
		t.Fatalf("want 0.12, got %v", got.CommissionRate)
	}
}

func TestMerchantServiceSetCommissionRateRejectsOutOfRange(t *testing.T) {
	svc := NewMerchantService(newFakeMerchantRepo())
	created, err := svc.Create(context.Background(), CreateMerchantInput{Name: "上游"})
	if err != nil {
		t.Fatalf("unexpected create error: %v", err)
	}

	if err := svc.SetCommissionRate(context.Background(), created.ID, 1); !errors.Is(err, ErrCommissionOutOfRange) {
		t.Fatalf("want ErrCommissionOutOfRange, got %v", err)
	}
}
