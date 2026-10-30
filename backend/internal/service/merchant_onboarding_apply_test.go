package service

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	infraerrors "github.com/woolsen/router2api/internal/pkg/errors"
)

// fakeApplyGroupRepo 同时实现 GroupRepository（供 Apply/ModelPlaza 使用）与
// MerchantOnboardingGroupRepository（供 AttachGroup 使用）。
type fakeApplyGroupRepo struct {
	GroupRepository

	mu     sync.Mutex
	nextID int64
	byID   map[int64]*Group
}

func newFakeApplyGroupRepo() *fakeApplyGroupRepo {
	return &fakeApplyGroupRepo{nextID: 1, byID: make(map[int64]*Group)}
}

func (r *fakeApplyGroupRepo) Create(_ context.Context, group *Group) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, existing := range r.byID {
		if existing.Name == group.Name {
			return ErrGroupExists
		}
	}
	group.ID = r.nextID
	r.nextID++
	cp := *group
	r.byID[group.ID] = &cp
	return nil
}

func (r *fakeApplyGroupRepo) GetByID(_ context.Context, id int64) (*Group, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	group, ok := r.byID[id]
	if !ok {
		return nil, ErrGroupNotFound
	}
	cp := *group
	return &cp, nil
}

func (r *fakeApplyGroupRepo) ExistsByName(_ context.Context, name string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, group := range r.byID {
		if group.Name == name {
			return true, nil
		}
	}
	return false, nil
}

func (r *fakeApplyGroupRepo) ListActive(_ context.Context) ([]Group, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Group, 0, len(r.byID))
	for _, group := range r.byID {
		if group.Status == StatusActive {
			out = append(out, *group)
		}
	}
	return out, nil
}

func (r *fakeApplyGroupRepo) ClaimMerchantOwnership(_ context.Context, merchantID, groupID int64) (bool, *int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	group, ok := r.byID[groupID]
	if !ok {
		return false, nil, ErrGroupNotFound
	}
	if group.MerchantID == nil {
		owner := merchantID
		group.MerchantID = &owner
		return true, &owner, nil
	}
	owner := *group.MerchantID
	return false, &owner, nil
}

// fakeApplyChannelRepo 是 EnsureGroupModels 所需的最小渠道仓储内存实现。
type fakeApplyChannelRepo struct {
	ChannelRepository

	mu     sync.Mutex
	nextID int64
	byID   map[int64]*Channel
}

func newFakeApplyChannelRepo() *fakeApplyChannelRepo {
	return &fakeApplyChannelRepo{nextID: 1, byID: make(map[int64]*Channel)}
}

func (r *fakeApplyChannelRepo) Create(_ context.Context, channel *Channel) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	channel.ID = r.nextID
	r.nextID++
	r.byID[channel.ID] = cloneApplyChannel(channel)
	return nil
}

func (r *fakeApplyChannelRepo) GetByID(_ context.Context, id int64) (*Channel, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	channel, ok := r.byID[id]
	if !ok {
		return nil, ErrChannelNotFound
	}
	return cloneApplyChannel(channel), nil
}

func (r *fakeApplyChannelRepo) Update(_ context.Context, channel *Channel) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byID[channel.ID]; !ok {
		return ErrChannelNotFound
	}
	r.byID[channel.ID] = cloneApplyChannel(channel)
	return nil
}

func (r *fakeApplyChannelRepo) ListAll(_ context.Context) ([]Channel, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Channel, 0, len(r.byID))
	for _, channel := range r.byID {
		out = append(out, *cloneApplyChannel(channel))
	}
	return out, nil
}

func (r *fakeApplyChannelRepo) ExistsByName(_ context.Context, name string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, channel := range r.byID {
		if channel.Name == name {
			return true, nil
		}
	}
	return false, nil
}

func (r *fakeApplyChannelRepo) GetChannelIDByGroupID(_ context.Context, groupID int64) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, channel := range r.byID {
		for _, gid := range channel.GroupIDs {
			if gid == groupID {
				return id, nil
			}
		}
	}
	return 0, nil
}

func (r *fakeApplyChannelRepo) GetGroupsInOtherChannels(context.Context, int64, []int64) ([]int64, error) {
	return nil, nil
}

func (r *fakeApplyChannelRepo) GetGroupPlatforms(context.Context, []int64) (map[int64]string, error) {
	return map[int64]string{}, nil
}

func cloneApplyChannel(channel *Channel) *Channel {
	cp := *channel
	cp.GroupIDs = append([]int64(nil), channel.GroupIDs...)
	cp.ModelPricing = append([]ChannelModelPricing(nil), channel.ModelPricing...)
	return &cp
}

// fakeApplyUserRepo 提供申请流程所需的用户读取与商户绑定能力。
type fakeApplyUserRepo struct {
	mu    sync.Mutex
	users map[int64]*User
}

func newFakeApplyUserRepo() *fakeApplyUserRepo {
	return &fakeApplyUserRepo{users: make(map[int64]*User)}
}

func (r *fakeApplyUserRepo) GetByID(_ context.Context, id int64) (*User, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	user, ok := r.users[id]
	if !ok {
		return nil, ErrUserNotFound
	}
	cp := *user
	return &cp, nil
}

func (r *fakeApplyUserRepo) PromoteToMerchant(_ context.Context, userID, merchantID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	user, ok := r.users[userID]
	if !ok {
		return ErrUserNotFound
	}
	id := merchantID
	user.Role = RoleMerchant
	user.MerchantID = &id
	return nil
}

type applyFixture struct {
	svc          *MerchantApplyService
	merchantRepo *fakeMerchantRepo
	groups       *fakeApplyGroupRepo
	channels     *fakeApplyChannelRepo
	users        *fakeApplyUserRepo
}

func newApplyFixture(t *testing.T, upstream http.Handler) *applyFixture {
	t.Helper()
	probe := newProbeTestServer(t, upstream)
	merchantRepo := newFakeMerchantRepo()
	merchantSvc := NewMerchantService(merchantRepo)
	groups := newFakeApplyGroupRepo()
	channels := newFakeApplyChannelRepo()
	users := newFakeApplyUserRepo()
	users.users[42] = &User{ID: 42, Email: "apply@example.com", Role: RoleUser, Status: StatusActive}

	channelSvc := NewChannelService(channels, groups, nil, nil, nil)
	onboarding := NewMerchantOnboardingService(merchantSvc, newFakeOnboardingAccountRepo(), groups, channelSvc, nil)
	apply := NewMerchantApplyService(probe, merchantSvc, groups, onboarding, users)

	return &applyFixture{
		svc:          apply,
		merchantRepo: merchantRepo,
		groups:       groups,
		channels:     channels,
		users:        users,
	}
}

func TestApplyPromotesUserToMerchantOnSuccess(t *testing.T) {
	ctx := context.Background()
	f := newApplyFixture(t, fakeOpenAIUpstream(t, true, true))

	merchant, err := f.svc.Submit(ctx, 42, ApplyInput{
		MerchantName: "一号",
		BaseURL:      probeTestBaseURL,
		APIKey:       "sk-probe",
		Platform:     PlatformOpenAI,
	})
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if merchant == nil || merchant.ID == 0 || merchant.Status != MerchantStatusActive {
		t.Fatalf("merchant = %+v, want active persisted merchant", merchant)
	}

	user, err := f.users.GetByID(ctx, 42)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if user.Role != RoleMerchant {
		t.Fatalf("role = %q, want merchant", user.Role)
	}
	if user.MerchantID == nil || *user.MerchantID != merchant.ID {
		t.Fatalf("merchant_id = %v, want %d", user.MerchantID, merchant.ID)
	}

	// 分组必须由申请流程创建，并由 AttachGroup 认领归属。
	var created *Group
	f.groups.mu.Lock()
	for _, group := range f.groups.byID {
		created = group
	}
	f.groups.mu.Unlock()
	if created == nil {
		t.Fatal("group was not created")
	}
	if created.Name != "一号" || created.Platform != PlatformOpenAI {
		t.Fatalf("group = %+v, want name/platform from request", created)
	}
	if created.Status != StatusActive || created.RateMultiplier != 1 {
		t.Fatalf("group status/rate = %s/%v, want active/1", created.Status, created.RateMultiplier)
	}
	if created.MerchantID == nil || *created.MerchantID != merchant.ID {
		t.Fatalf("group merchant_id = %v, want %d", created.MerchantID, merchant.ID)
	}

	// 分组必须通过 ModelPlazaService 出现在货架上（模型已写入渠道）。
	shelf := NewModelPlazaService(f.channels, f.groups, nil, nil, nil)
	plazaGroups, err := shelf.ListGroups(ctx)
	if err != nil {
		t.Fatalf("ListGroups() error = %v", err)
	}
	if len(plazaGroups) != 1 {
		t.Fatalf("plaza groups = %d, want 1", len(plazaGroups))
	}
	if plazaGroups[0].ID != created.ID || plazaGroups[0].MerchantID == nil || *plazaGroups[0].MerchantID != merchant.ID {
		t.Fatalf("plaza group = %+v, want applied merchant group", plazaGroups[0])
	}
	if len(plazaGroups[0].Models) == 0 || plazaGroups[0].Models[0].Name != "gpt-4o-mini" {
		t.Fatalf("plaza models = %+v, want probed model list", plazaGroups[0].Models)
	}
}

func TestApplyRejectsProbeFailureWithoutCreatingMerchant(t *testing.T) {
	ctx := context.Background()
	f := newApplyFixture(t, fakeOpenAIUpstream(t, false, true))

	_, err := f.svc.Submit(ctx, 42, ApplyInput{
		MerchantName: "一号",
		BaseURL:      probeTestBaseURL,
		APIKey:       "sk-probe",
		Platform:     PlatformOpenAI,
	})
	if err == nil || err.Error() != "upstream_returns_no_usage" {
		t.Fatalf("Submit() error = %v, want upstream_returns_no_usage", err)
	}
	if len(f.merchantRepo.items) != 0 {
		t.Fatalf("merchants = %d, probe failure must not create a merchant", len(f.merchantRepo.items))
	}
	f.groups.mu.Lock()
	groupCount := len(f.groups.byID)
	f.groups.mu.Unlock()
	if groupCount != 0 {
		t.Fatalf("groups = %d, probe failure must not create a group", groupCount)
	}
	user, _ := f.users.GetByID(ctx, 42)
	if user.Role != RoleUser || user.MerchantID != nil {
		t.Fatalf("user = %+v, probe failure must not promote", user)
	}
}

func TestApplyRejectsSecondApplication(t *testing.T) {
	ctx := context.Background()
	f := newApplyFixture(t, fakeOpenAIUpstream(t, true, true))
	input := ApplyInput{
		MerchantName: "一号",
		BaseURL:      probeTestBaseURL,
		APIKey:       "sk-probe",
		Platform:     PlatformOpenAI,
	}
	if _, err := f.svc.Submit(ctx, 42, input); err != nil {
		t.Fatalf("first Submit() error = %v", err)
	}
	input.MerchantName = "二号"
	_, err := f.svc.Submit(ctx, 42, input)
	if err == nil {
		t.Fatal("second Submit() must be rejected")
	}
	if infraerrors.Reason(err) != "MERCHANT_ALREADY_BOUND" {
		t.Fatalf("second Submit() error = %v, want MERCHANT_ALREADY_BOUND", err)
	}
}

// TestApplyUsesRequestedModelsAndRate 覆盖修正项“名称/platform/rate/models 来自请求”。
func TestApplyUsesRequestedModelsAndRate(t *testing.T) {
	ctx := context.Background()
	f := newApplyFixture(t, fakeOpenAIUpstream(t, true, true))
	rate := 0.6
	if _, err := f.svc.Submit(ctx, 42, ApplyInput{
		MerchantName:   "费率商户",
		BaseURL:        probeTestBaseURL,
		APIKey:         "sk-probe",
		Platform:       PlatformOpenAI,
		RateMultiplier: rate,
		Models:         []string{"gpt-4o-mini"},
	}); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	f.groups.mu.Lock()
	var group *Group
	for _, item := range f.groups.byID {
		group = item
	}
	f.groups.mu.Unlock()
	if group == nil || group.RateMultiplier != rate {
		t.Fatalf("group = %+v, want rate %v from request", group, rate)
	}
	shelf := NewModelPlazaService(f.channels, f.groups, nil, nil, nil)
	plazaGroups, err := shelf.ListGroups(ctx)
	if err != nil {
		t.Fatalf("ListGroups() error = %v", err)
	}
	if len(plazaGroups) != 1 || len(plazaGroups[0].Models) != 1 || plazaGroups[0].Models[0].Name != "gpt-4o-mini" {
		t.Fatalf("models = %+v, want requested model only", plazaGroups)
	}
}

func TestMerchantIsNewForSevenDays(t *testing.T) {
	created := time.Now().Add(-6 * 24 * time.Hour)
	merchant := &Merchant{CreatedAt: created}
	if !merchant.IsNewAt(created.Add(6 * 24 * time.Hour)) {
		t.Fatal("merchant created 6 days ago must still be new")
	}
	if merchant.IsNewAt(created.Add(7*24*time.Hour + time.Second)) {
		t.Fatal("merchant older than 7 days must not be new")
	}
	if (&Merchant{}).IsNewAt(time.Now()) {
		t.Fatal("merchant without created_at must not be labelled new")
	}
}
