package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeOnboardingAccountRepo 是 AttachGroup 所需的账号仓储内存实现。
// 它模拟真实仓储的关键语义：
//   - GetMerchantGroupAccount 不按状态过滤（error/disabled 也要复用）；
//   - WithMerchantGroupLock 按 (merchant_id, group_id) 串行化；
//   - 可注入查重后的停顿，用来放大并发窗口。
type fakeOnboardingAccountRepo struct {
	mu             sync.Mutex
	nextID         int64
	accounts       map[int64]*Account
	groups         map[int64][]AccountGroup
	locks          map[[2]int64]*sync.Mutex
	createCalls    int
	updateCalls    int
	lockDepth      int
	getOutsideLock int

	// getSnapshotHook 在按 (merchant_id, group_id) 查重读完快照、返回之前调用（不持锁）。
	getSnapshotHook func()
}

func newFakeOnboardingAccountRepo() *fakeOnboardingAccountRepo {
	return &fakeOnboardingAccountRepo{
		nextID:   1,
		accounts: make(map[int64]*Account),
		groups:   make(map[int64][]AccountGroup),
		locks:    make(map[[2]int64]*sync.Mutex),
	}
}

// seed 直接写入一个已存在的账号及其分组绑定，模拟历史数据。
func (r *fakeOnboardingAccountRepo) seed(account *Account, groupID int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.accounts[account.ID] = cloneOnboardingAccount(account)
	if account.ID >= r.nextID {
		r.nextID = account.ID + 1
	}
	r.groups[groupID] = append(r.groups[groupID], AccountGroup{
		AccountID: account.ID,
		GroupID:   groupID,
		Priority:  50,
	})
}

func (r *fakeOnboardingAccountRepo) bindingCount(groupID int64) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.groups[groupID])
}

func (r *fakeOnboardingAccountRepo) bindingsFor(groupID int64) []AccountGroup {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]AccountGroup(nil), r.groups[groupID]...)
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

func (r *fakeOnboardingAccountRepo) GetMerchantGroupAccount(_ context.Context, merchantID, groupID int64) (*Account, error) {
	r.mu.Lock()
	var found *Account
	for _, binding := range r.groups[groupID] {
		acc, ok := r.accounts[binding.AccountID]
		if ok && acc.MerchantID != nil && *acc.MerchantID == merchantID {
			found = cloneOnboardingAccount(acc)
			break
		}
	}
	inLock := r.lockDepth > 0
	if !inLock {
		r.getOutsideLock++
	}
	r.mu.Unlock()
	if r.getSnapshotHook != nil {
		r.getSnapshotHook()
	}
	return found, nil
}

func (r *fakeOnboardingAccountRepo) WithMerchantGroupLock(
	_ context.Context,
	merchantID, groupID int64,
	fn func(context.Context) error,
) error {
	key := [2]int64{merchantID, groupID}
	r.mu.Lock()
	lock := r.locks[key]
	if lock == nil {
		lock = &sync.Mutex{}
		r.locks[key] = lock
	}
	r.mu.Unlock()

	lock.Lock()
	defer lock.Unlock()

	r.mu.Lock()
	r.lockDepth++
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.lockDepth--
		r.mu.Unlock()
	}()

	return fn(context.Background())
}

func (r *fakeOnboardingAccountRepo) getOutsideLockCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.getOutsideLock
}

func (r *fakeOnboardingAccountRepo) CreateWithAccountGroups(_ context.Context, account *Account, groups []AccountGroup) error {
	r.mu.Lock()
	defer r.mu.Unlock()
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
	r.mu.Lock()
	defer r.mu.Unlock()
	r.updateCalls++
	r.accounts[account.ID] = cloneOnboardingAccount(account)
	return nil
}

func (r *fakeOnboardingAccountRepo) createCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.createCalls
}

func (r *fakeOnboardingAccountRepo) updateCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.updateCalls
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

	bindings := f.accounts.bindingsFor(7)
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
	if got := f.accounts.createCount(); got != 1 {
		t.Fatalf("want exactly 1 create, got %d", got)
	}
	if got := f.accounts.updateCount(); got != 1 {
		t.Fatalf("want exactly 1 update, got %d", got)
	}
	if got := f.accounts.bindingCount(7); got != 1 {
		t.Fatalf("want 1 account_groups binding, got %d", got)
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

func TestAttachGroupReusesNonActiveAccount(t *testing.T) {
	for _, status := range []string{StatusError, StatusDisabled} {
		t.Run(status, func(t *testing.T) {
			ctx := context.Background()
			f := newOnboardingFixture(t, nil)
			merchantID := f.merchant.ID
			f.accounts.seed(&Account{
				ID:          99,
				Name:        "merchant:" + f.merchant.Name,
				Platform:    PlatformOpenAI,
				Type:        AccountTypeAPIKey,
				Credentials: map[string]any{"base_url": "https://old.example.com/v1", "api_key": "sk-old"},
				Status:      status,
				Schedulable: false,
				MerchantID:  &merchantID,
			}, 7)

			acc, err := f.svc.AttachGroup(ctx, AttachGroupInput{
				MerchantID: f.merchant.ID,
				GroupID:    7,
				BaseURL:    "https://up.example.com/v1",
				APIKey:     "sk-new",
				Platform:   PlatformOpenAI,
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if acc.ID != 99 {
				t.Fatalf("must reuse the %s account, got new account %d", status, acc.ID)
			}
			if got := acc.Credentials["api_key"]; got != "sk-new" {
				t.Fatalf("key not rotated, got %v", got)
			}
			if got := f.accounts.createCount(); got != 0 {
				t.Fatalf("must not create a second account, got %d creates", got)
			}
			if got := f.accounts.bindingCount(7); got != 1 {
				t.Fatalf("must not add a second binding, got %d", got)
			}
		})
	}
}

func TestAttachGroupConcurrentCallsCreateSingleAccount(t *testing.T) {
	ctx := context.Background()
	f := newOnboardingFixture(t, nil)
	f.accounts.getSnapshotHook = func() {
		// 放大“查到没有”到“插入”之间的窗口；没有锁时两个调用都会漏查。
		time.Sleep(20 * time.Millisecond)
	}

	keys := []string{"sk-0", "sk-1"}
	ids := make(chan int64, len(keys))
	errs := make(chan error, len(keys))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, key := range keys {
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			<-start
			acc, err := f.svc.AttachGroup(ctx, AttachGroupInput{
				MerchantID: f.merchant.ID,
				GroupID:    7,
				BaseURL:    "https://up.example.com/v1",
				APIKey:     key,
				Platform:   PlatformOpenAI,
			})
			if err != nil {
				errs <- err
				return
			}
			ids <- acc.ID
		}(key)
	}
	close(start)
	wg.Wait()
	close(ids)
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent attach: %v", err)
		}
	}
	if got := f.accounts.createCount(); got != 1 {
		t.Fatalf("want exactly 1 account created, got %d", got)
	}
	if got := f.accounts.bindingCount(7); got != 1 {
		t.Fatalf("want exactly 1 account_groups binding, got %d", got)
	}
	if got := f.accounts.getOutsideLockCount(); got != 0 {
		t.Fatalf("merchant lookup must happen inside the lock, got %d lookups outside", got)
	}

	seen := make(map[int64]struct{}, len(keys))
	for id := range ids {
		seen[id] = struct{}{}
	}
	if len(seen) != 1 {
		t.Fatalf("want both calls to reuse one account, got IDs %v", seen)
	}
}
