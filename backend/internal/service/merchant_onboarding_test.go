package service

import (
	"context"
	"errors"
	"testing"
)

// fakeOnboardingAccountRepo 是 AttachGroup 所需的账号仓储内存实现。
type fakeOnboardingAccountRepo struct {
	nextID      int64
	accounts    map[int64]*Account
	groups      map[int64][]AccountGroup
	createCalls int
	updateCalls int
}

func newFakeOnboardingAccountRepo() *fakeOnboardingAccountRepo {
	return &fakeOnboardingAccountRepo{
		nextID:   1,
		accounts: make(map[int64]*Account),
		groups:   make(map[int64][]AccountGroup),
	}
}

func cloneOnboardingAccount(in *Account) *Account {
	if in == nil {
		return nil
	}
	cp := *in
	cp.Credentials = make(map[string]any, len(in.Credentials))
	for k, v := range in.Credentials {
		cp.Credentials[k] = v
	}
	return &cp
}

func (r *fakeOnboardingAccountRepo) ListByGroup(_ context.Context, groupID int64) ([]Account, error) {
	out := make([]Account, 0, len(r.groups[groupID]))
	for _, binding := range r.groups[groupID] {
		if acc, ok := r.accounts[binding.AccountID]; ok {
			out = append(out, *cloneOnboardingAccount(acc))
		}
	}
	return out, nil
}

func (r *fakeOnboardingAccountRepo) CreateWithAccountGroups(_ context.Context, account *Account, groups []AccountGroup) error {
	r.createCalls++
	account.ID = r.nextID
	r.nextID++
	r.accounts[account.ID] = cloneOnboardingAccount(account)
	for _, binding := range groups {
		binding.AccountID = account.ID
		r.groups[binding.GroupID] = append(r.groups[binding.GroupID], binding)
	}
	return nil
}

func (r *fakeOnboardingAccountRepo) Update(_ context.Context, account *Account) error {
	r.updateCalls++
	r.accounts[account.ID] = cloneOnboardingAccount(account)
	return nil
}

// fakeOnboardingGroupRepo 仅实现接入流程用到的按 ID 查询。
type fakeOnboardingGroupRepo struct {
	byID map[int64]*Group
}

func newFakeOnboardingGroupRepo() *fakeOnboardingGroupRepo {
	return &fakeOnboardingGroupRepo{byID: make(map[int64]*Group)}
}

func (r *fakeOnboardingGroupRepo) GetByID(_ context.Context, id int64) (*Group, error) {
	group, ok := r.byID[id]
	if !ok {
		return nil, ErrGroupNotFound
	}
	cp := *group
	return &cp, nil
}

type onboardingFixture struct {
	svc          *MerchantOnboardingService
	merchantRepo *fakeMerchantRepo
	merchant     *Merchant
	accounts     *fakeOnboardingAccountRepo
	groups       *fakeOnboardingGroupRepo
}

func newOnboardingFixture(t *testing.T, groupMerchantID *int64) *onboardingFixture {
	t.Helper()
	merchantRepo := newFakeMerchantRepo()
	merchantSvc := NewMerchantService(merchantRepo)
	merchant, err := merchantSvc.Create(context.Background(), CreateMerchantInput{Name: "上游一号"})
	if err != nil {
		t.Fatalf("create merchant: %v", err)
	}

	accounts := newFakeOnboardingAccountRepo()
	groups := newFakeOnboardingGroupRepo()
	groups.byID[7] = &Group{
		ID:             7,
		Name:           "merchant-group",
		Platform:       PlatformOpenAI,
		RateMultiplier: 0.2,
		MerchantID:     groupMerchantID,
	}

	return &onboardingFixture{
		svc:          NewMerchantOnboardingService(merchantSvc, accounts, groups),
		merchantRepo: merchantRepo,
		merchant:     merchant,
		accounts:     accounts,
		groups:       groups,
	}
}

func TestAttachGroupCreatesUpstreamAccount(t *testing.T) {
	ctx := context.Background()
	f := newOnboardingFixture(t, nil)

	acc, err := f.svc.AttachGroup(ctx, AttachGroupInput{
		MerchantID: f.merchant.ID,
		GroupID:    7,
		BaseURL:    "https://up.example.com/v1",
		APIKey:     "sk-up",
		Platform:   PlatformOpenAI,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if acc.Type != AccountTypeAPIKey {
		t.Fatalf("want %s, got %s", AccountTypeAPIKey, acc.Type)
	}
	if got := acc.Credentials["base_url"]; got != "https://up.example.com/v1" {
		t.Fatalf("base_url not stored, got %v", got)
	}
	if got := acc.Credentials["api_key"]; got != "sk-up" {
		t.Fatalf("api_key not stored, got %v", got)
	}
	if acc.RateMultiplier != nil && *acc.RateMultiplier != 1.0 {
		t.Fatalf("account rate must stay 1.0, got %v", *acc.RateMultiplier)
	}
	if acc.MerchantID == nil || *acc.MerchantID != f.merchant.ID {
		t.Fatal("merchant not bound")
	}
	if !acc.Schedulable {
		t.Fatal("upstream account must be schedulable")
	}
	if want := "merchant:" + f.merchant.Name; acc.Name != want {
		t.Fatalf("want name %q, got %q", want, acc.Name)
	}

	bindings := f.accounts.groups[7]
	if len(bindings) != 1 {
		t.Fatalf("want 1 account_groups binding, got %d", len(bindings))
	}
	if bindings[0].AccountID != acc.ID {
		t.Fatalf("binding points at account %d, want %d", bindings[0].AccountID, acc.ID)
	}
	if bindings[0].Priority != 50 {
		t.Fatalf("want priority 50, got %d", bindings[0].Priority)
	}
}

func TestAttachGroupIsIdempotentAndRotatesCredentials(t *testing.T) {
	ctx := context.Background()
	f := newOnboardingFixture(t, nil)

	first, err := f.svc.AttachGroup(ctx, AttachGroupInput{
		MerchantID: f.merchant.ID,
		GroupID:    7,
		BaseURL:    "https://up.example.com/v1",
		APIKey:     "sk-up",
		Platform:   PlatformOpenAI,
	})
	if err != nil {
		t.Fatalf("first attach: %v", err)
	}

	second, err := f.svc.AttachGroup(ctx, AttachGroupInput{
		MerchantID: f.merchant.ID,
		GroupID:    7,
		BaseURL:    "https://up2.example.com/v1",
		APIKey:     "sk-new",
		Platform:   PlatformOpenAI,
	})
	if err != nil {
		t.Fatalf("second attach: %v", err)
	}

	if first.ID != second.ID {
		t.Fatalf("must update in place, not create a second account: %d != %d", first.ID, second.ID)
	}
	if got := second.Credentials["api_key"]; got != "sk-new" {
		t.Fatalf("key not rotated, got %v", got)
	}
	if got := second.Credentials["base_url"]; got != "https://up2.example.com/v1" {
		t.Fatalf("base_url not rotated, got %v", got)
	}
	if f.accounts.createCalls != 1 {
		t.Fatalf("want exactly 1 create, got %d", f.accounts.createCalls)
	}
	if f.accounts.updateCalls != 1 {
		t.Fatalf("want exactly 1 update, got %d", f.accounts.updateCalls)
	}
	if bindings := f.accounts.groups[7]; len(bindings) != 1 {
		t.Fatalf("want 1 account_groups binding, got %d", len(bindings))
	}
}

func TestAttachGroupRejectsPlatformMismatch(t *testing.T) {
	ctx := context.Background()
	f := newOnboardingFixture(t, nil)
	f.groups.byID[7].Platform = PlatformAnthropic

	_, err := f.svc.AttachGroup(ctx, AttachGroupInput{
		MerchantID: f.merchant.ID,
		GroupID:    7,
		BaseURL:    "https://up.example.com/v1",
		APIKey:     "sk-up",
		Platform:   PlatformOpenAI,
	})
	if !errors.Is(err, ErrPlatformMismatch) {
		t.Fatalf("want ErrPlatformMismatch, got %v", err)
	}
}

func TestAttachGroupRejectsUnsupportedPlatform(t *testing.T) {
	ctx := context.Background()
	f := newOnboardingFixture(t, nil)

	_, err := f.svc.AttachGroup(ctx, AttachGroupInput{
		MerchantID: f.merchant.ID,
		GroupID:    7,
		BaseURL:    "https://up.example.com/v1",
		APIKey:     "sk-up",
		Platform:   "gemini",
	})
	if !errors.Is(err, ErrPlatformMismatch) {
		t.Fatalf("want ErrPlatformMismatch, got %v", err)
	}
}

func TestAttachGroupRejectsSuspendedMerchant(t *testing.T) {
	ctx := context.Background()
	f := newOnboardingFixture(t, nil)
	f.merchantRepo.items[f.merchant.ID].Status = MerchantStatusSuspended

	_, err := f.svc.AttachGroup(ctx, AttachGroupInput{
		MerchantID: f.merchant.ID,
		GroupID:    7,
		BaseURL:    "https://up.example.com/v1",
		APIKey:     "sk-up",
		Platform:   PlatformOpenAI,
	})
	if !errors.Is(err, ErrMerchantNotActive) {
		t.Fatalf("want ErrMerchantNotActive, got %v", err)
	}
}

func TestAttachGroupRejectsForeignGroup(t *testing.T) {
	ctx := context.Background()
	otherMerchantID := int64(99)
	f := newOnboardingFixture(t, &otherMerchantID)

	_, err := f.svc.AttachGroup(ctx, AttachGroupInput{
		MerchantID: f.merchant.ID,
		GroupID:    7,
		BaseURL:    "https://up.example.com/v1",
		APIKey:     "sk-up",
		Platform:   PlatformOpenAI,
	})
	if !errors.Is(err, ErrMerchantGroupOwnership) {
		t.Fatalf("want ErrMerchantGroupOwnership, got %v", err)
	}
}
