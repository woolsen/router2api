package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/woolsen/router2api/internal/config"
	middleware2 "github.com/woolsen/router2api/internal/server/middleware"
	"github.com/woolsen/router2api/internal/service"

	"github.com/gin-gonic/gin"
)

// 本文件用真实 ChatCompletions 链路复现「单账号 + 利润否决 → 选号耗尽」，
// 确保 200 心跳后的失败分支被指标归因为上游失败。

type chainGroupRepo struct {
	service.GroupRepository
	group *service.Group
}

func (r *chainGroupRepo) GetByID(context.Context, int64) (*service.Group, error) {
	return r.group, nil
}

func (r *chainGroupRepo) GetByIDLite(context.Context, int64) (*service.Group, error) {
	return r.group, nil
}

// chainSchedulerCache 让选号快照与终检快照返回不同费率的同一账号：
// 选号时费率合格，槽位后终检读到更新更贵的最新快照 → 利润否决。
type chainSchedulerCache struct {
	service.SchedulerCache
	snapshot []*service.Account
	latest   map[int64]*service.Account
}

func (c *chainSchedulerCache) GetSnapshot(context.Context, service.SchedulerBucket) ([]*service.Account, bool, error) {
	return c.snapshot, true, nil
}

func (c *chainSchedulerCache) GetAccount(_ context.Context, accountID int64) (*service.Account, error) {
	return c.latest[accountID], nil
}

// chainSlowUserSlotCache 首次用户槽获取返回失败并延迟，逼出并发等待期的 SSE 心跳，
// 后续获取成功。
type chainSlowUserSlotCache struct {
	service.ConcurrencyCache
	mu       sync.Mutex
	acquires int
}

func (c *chainSlowUserSlotCache) AcquireUserSlot(context.Context, int64, int, string) (bool, error) {
	c.mu.Lock()
	c.acquires++
	attempt := c.acquires
	c.mu.Unlock()
	if attempt == 1 {
		time.Sleep(60 * time.Millisecond)
		return false, nil
	}
	return true, nil
}

func (c *chainSlowUserSlotCache) ReleaseUserSlot(context.Context, int64, string) error { return nil }
func (c *chainSlowUserSlotCache) IncrementWaitCount(context.Context, int64, int) (bool, error) {
	return true, nil
}
func (c *chainSlowUserSlotCache) DecrementWaitCount(context.Context, int64) error { return nil }
func (c *chainSlowUserSlotCache) GetUserConcurrency(context.Context, int64) (int, error) {
	return 0, nil
}

func TestCCChatCompletionsProfitVetoSelectionExhaustionAfterHeartbeat(t *testing.T) {
	gin.SetMode(gin.TestMode)

	groupID := int64(9400)
	accountID := int64(9401)
	eligibleRate := 0.1
	vetoRate := 2.0
	now := time.Now()

	group := &service.Group{
		ID:                   groupID,
		Hydrated:             true,
		Platform:             service.PlatformAnthropic,
		Status:               service.StatusActive,
		RateMultiplier:       1,
		SubscriptionType:     service.SubscriptionTypeStandard,
		ProfitControlEnabled: true,
		ProfitMinMargin:      0.5,
		ProfitSafetyBuffer:   0,
	}
	eligibleAccount := &service.Account{
		ID:             accountID,
		Platform:       service.PlatformAnthropic,
		Type:           service.AccountTypeAPIKey,
		Status:         service.StatusActive,
		Schedulable:    true,
		Concurrency:    1,
		RateMultiplier: &eligibleRate,
		UpdatedAt:      now.Add(-time.Minute),
		Credentials:    map[string]any{"api_key": "test-key"},
		AccountGroups:  []service.AccountGroup{{AccountID: accountID, GroupID: groupID}},
	}
	vetoAccount := *eligibleAccount
	vetoAccount.RateMultiplier = &vetoRate
	vetoAccount.UpdatedAt = now

	schedulerCache := &chainSchedulerCache{
		snapshot: []*service.Account{eligibleAccount},
		latest:   map[int64]*service.Account{accountID: &vetoAccount},
	}
	snapshot := service.NewSchedulerSnapshotService(schedulerCache, nil, nil, nil, nil)

	cfg := &config.Config{RunMode: config.RunModeSimple}
	usageRepo := &openAIWSUsageHandlerUsageLogRepoStub{created: make(chan *service.UsageLog, 2)}
	billingCache := service.NewBillingCacheService(newHandlerInflightCache(100), nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingCache.Stop)

	gw := service.NewGatewayService(
		nil, // accountRepo (not used: scheduler snapshot hit)
		&chainGroupRepo{group: group},
		usageRepo,
		nil, // usageBillingRepo
		nil, // userRepo
		nil, // userSubRepo
		nil, // userGroupRateRepo
		nil, // cache (disable sticky)
		cfg,
		snapshot,
		nil, // concurrencyService (disable load-aware; tryAcquire always acquired)
		nil, // billingService
		nil, // rateLimitService
		nil, // billingCacheService
		nil, // identityService
		nil, // httpUpstream
		nil, // deferredService
		nil, // claudeTokenProvider
		nil, // sessionLimitCache
		nil, // rpmCache
		nil, // digestStore
		nil, // settingService
		nil, // tlsFPProfileService
		nil, // channelService
		nil, // resolver
		nil, // compositeResolver
		nil, // balanceNotifyService
		nil, // userPlatformQuotaRepo
		nil,
	)

	metricRepo := &recorderTestMetricRepo{}
	metricsSvc := service.NewMetricsService(metricRepo, service.MetricsOptions{Buffer: 8, FlushInterval: time.Millisecond})
	metricsSvc.Start()

	concurrencyHelper := NewConcurrencyHelper(
		service.NewConcurrencyService(&chainSlowUserSlotCache{}),
		SSEPingFormatClaude,
		10*time.Millisecond,
	)
	h := &GatewayHandler{
		gatewayService:      gw,
		billingCacheService: billingCache,
		concurrencyHelper:   concurrencyHelper,
		cfg:                 cfg,
		maxAccountSwitches:  1,
		metricsService:      metricsSvc,
	}

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	body := `{"model":"claude-sonnet-4.5","stream":true,"messages":[{"role":"user","content":"hello"}]}`
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	apiKey := &service.APIKey{
		ID:      9402,
		UserID:  9403,
		GroupID: &groupID,
		Group:   group,
		User:    &service.User{ID: 9403, Balance: 100},
	}
	c.Set(string(middleware2.ContextKeyAPIKey), apiKey)
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 9403, Concurrency: 1})

	h.ChatCompletions(c)

	rows := flushRecorderMetrics(t, metricsSvc, metricRepo)
	if len(rows) != 1 {
		t.Fatalf("want 1 request metric, got %d", len(rows))
	}
	row := rows[0]
	if row.StatusCode != http.StatusOK {
		t.Fatalf("status_code=%d, want wire status 200 (heartbeat committed)", row.StatusCode)
	}
	if !row.UpstreamError {
		t.Fatal("profit-veto selection exhaustion after heartbeat must be upstream_error")
	}

	out := recorder.Body.String()
	if !strings.Contains(out, ": ping") && !strings.Contains(out, `"type": "ping"`) {
		t.Fatalf("test precondition failed: no slot-wait heartbeat in body: %q", out)
	}
	if got := strings.Count(out, `"error"`); got != 1 {
		t.Fatalf("want exactly one stream error frame, got %d: %q", got, out)
	}
	if got := strings.Count(out, "data: [DONE]"); got != 1 {
		t.Fatalf("want exactly one [DONE] terminal frame, got %d: %q", got, out)
	}
	if !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Fatalf("stream must end with [DONE], body=%q", out)
	}
}
