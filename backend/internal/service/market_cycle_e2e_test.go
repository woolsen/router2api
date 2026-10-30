//go:build !unit

// TestMarketCycleE2E 需要真实 PostgreSQL（testcontainers），因此不参与
// -tags=unit 的单元测试套件；没有 Docker 时自动跳过。
//
// 这是一条按周运营闭环的真实演练：Day0 商户自助入驻 → Day1–3 网关流量
// → Day8 结算 → Day9 申诉/调账 → 提现/打款。时间通过注入 clock 推进，
// 所有金额、状态与流水都来自真实数据库，不手写余额。
package service_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/woolsen/router2api/internal/config"
	"github.com/woolsen/router2api/internal/pkg/timezone"
	"github.com/woolsen/router2api/internal/pkg/tlsfingerprint"
	"github.com/woolsen/router2api/internal/repository"
	"github.com/woolsen/router2api/internal/service"
)

const (
	cycleCommission    = 0.125
	cycleInputTokens   = 1_000_000
	cycleOutputTokens  = 100_000
	cycleCostPerCall   = 4.0 // 1M × $2.5/MTok + 100k × $15/MTok
	cycleSuccessCount  = 47
	cycleFailureCount  = 3
	cycleRequestCount  = cycleSuccessCount + cycleFailureCount
	cycleUpstreamModel = "gpt-5.4"
	cycleFailMarker    = "cycle_upstream_failure"
)

// cycleClock 是测试用的可推进时钟。服务只读取 Now，测试显式推进日期，
// 不依赖真实时间，也不 sleep。
type cycleClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *cycleClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *cycleClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// cycleHTTPUpstream 让真实网关通过 httptest TLS 桩的客户端发请求。
// 桩的 Transport 把公网域名拨到本地监听地址，证书由上游自己签发。
type cycleHTTPUpstream struct {
	client *http.Client
}

func (u *cycleHTTPUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return u.client.Do(req)
}

func (u *cycleHTTPUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, concurrency)
}

type cycleLedgerEntry struct {
	ID        int64
	Type      string
	Amount    float64
	RefType   string
	RefID     int64
	CreatedAt time.Time
}

func TestMarketCycleE2E(t *testing.T) {
	ctx := context.Background()
	require.NoError(t, timezone.Init("UTC"))
	client, db := marketE2EDatabase(t, ctx)

	// 自然周从 Day0（周一 00:00 UTC）开始；Day8 时该周已封账 24h。
	day0 := time.Date(2026, 1, 5, 10, 0, 0, 0, time.UTC)
	day1 := day0.AddDate(0, 0, 1)
	day2 := day0.AddDate(0, 0, 2)
	day3 := day0.AddDate(0, 0, 3)
	day3Traffic := time.Date(day3.Year(), day3.Month(), day3.Day(), 11, 50, 0, 0, time.UTC) // Day3 最后一个 15 分钟窗口
	day3Health := day3Traffic.Truncate(time.Hour).Add(time.Hour)
	day8 := day0.AddDate(0, 0, 8)
	day9 := day0.AddDate(0, 0, 9)
	weekStart := startOfUTCWeek(day0)
	weekEnd := weekStart.AddDate(0, 0, 7)
	require.True(t, day0.After(weekStart), "Day0 must fall inside the first settlement week")

	clock := &cycleClock{now: day0}

	// ---- fake 商户上游：探测（models/usage/stream）+ 网关 47 成功 + 3 个 502 ----
	var upstreamMu sync.Mutex
	var gatewayPosts int
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":[{"id":"`+cycleUpstreamModel+`"}]}`)
			return
		case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
			upstreamMu.Lock()
			gatewayPosts++
			requestSeq := gatewayPosts
			upstreamMu.Unlock()
			w.Header().Set("x-request-id", fmt.Sprintf("req-cycle-upstream-%d", requestSeq))
		default:
			http.NotFound(w, r)
			return
		}

		if bytes.Contains(body, []byte(cycleFailMarker)) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, `{"error":{"message":"synthetic upstream outage","type":"upstream_error"}}`)
			return
		}

		var req struct {
			Stream bool   `json:"stream"`
			Model  string `json:"model"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			first := fmt.Sprintf(
				`data: {"id":"chatcmpl-cycle","object":"chat.completion.chunk","model":%q,"choices":[{"index":0,"delta":{"role":"assistant","content":"cycle"},"finish_reason":null}]}`,
				cycleUpstreamModel)
			last := fmt.Sprintf(
				`data: {"id":"chatcmpl-cycle","object":"chat.completion.chunk","model":%q,"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`,
				cycleUpstreamModel, cycleInputTokens, cycleOutputTokens, cycleInputTokens+cycleOutputTokens)
			_, _ = io.WriteString(w, first+"\n\n")
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			_, _ = io.WriteString(w, last+"\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, fmt.Sprintf(
			`{"id":"chatcmpl-cycle","object":"chat.completion","model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":"cycle"},"finish_reason":"stop"}],"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`,
			cycleUpstreamModel, cycleInputTokens, cycleOutputTokens, cycleInputTokens+cycleOutputTokens))
	}))
	defer upstream.Close()

	// 公网域名 + Transport 拨号到本地 TLS 桩：探测的 https 校验与网关的
	// base_url 校验都按真实规则执行，只有 DNS/拨号被固定到本地。
	tlsTransport, ok := upstream.Client().Transport.(*http.Transport)
	require.True(t, ok, "httptest TLS client transport must be *http.Transport")
	transport := tlsTransport.Clone()
	target := upstream.Listener.Addr().String()
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return dialer.DialContext(ctx, network, target)
	}
	httpClient := &http.Client{Transport: transport, Timeout: 20 * time.Second}
	const upstreamBaseURL = "https://upstream.example.com"

	// ---- 真实仓储 ----
	merchantRepo := repository.NewMerchantRepository(client)
	groupRepo := repository.NewGroupRepository(client, db)
	accountRepo := repository.NewAccountRepository(client, db, nil)
	channelRepo := repository.NewChannelRepository(db)
	userRepo := repository.NewUserRepository(client, db)
	apiKeyRepo := repository.NewAPIKeyRepository(client, db)
	usageLogRepo := repository.NewUsageLogRepository(client, db)
	usageBillingRepo := repository.NewUsageBillingRepository(client, db)
	userSubRepo := repository.NewUserSubscriptionRepository(client)
	userGroupRateRepo := repository.NewUserGroupRateRepository(db)
	settlementRepo := repository.NewSettlementRepository(client, db)
	settlementOpsRepo := repository.NewSettlementOpsRepository(client, db)
	groupMetricRepo := repository.NewGroupMetricRepository(db)
	withdrawalRepo := repository.NewWithdrawalRepository(db)
	disputeRepo := repository.NewDisputeRepository(db)

	onboardingAccountRepo, ok := accountRepo.(service.MerchantOnboardingAccountRepository)
	require.True(t, ok)
	onboardingGroupRepo, ok := groupRepo.(service.MerchantOnboardingGroupRepository)
	require.True(t, ok)

	cfg := &config.Config{RunMode: config.RunModeStandard}
	cfg.Default.RateMultiplier = 1

	merchantSvc := service.NewMerchantService(merchantRepo)
	channelSvc := service.NewChannelService(channelRepo, groupRepo, nil, nil, nil)
	onboardingSvc := service.NewMerchantOnboardingService(
		merchantSvc, onboardingAccountRepo, onboardingGroupRepo, channelSvc, nil)
	applyUserRepo, ok := userRepo.(service.MerchantApplyUserRepository)
	require.True(t, ok, "user repository must support merchant self-serve apply")
	applySvc := service.NewMerchantApplyService(
		service.NewProbeServiceForTest(httpClient), merchantSvc, groupRepo, onboardingSvc, applyUserRepo)

	// ---- Day 0：商户自助申请，探测通过后自动上架 ----
	applicant := &service.User{
		Email:        "cycle-applicant@example.com",
		PasswordHash: "test-password-hash",
		Role:         service.RoleUser,
		Status:       service.StatusActive,
		Concurrency:  5,
	}
	require.NoError(t, userRepo.Create(ctx, applicant))

	merchant, err := applySvc.Submit(ctx, applicant.ID, service.ApplyInput{
		MerchantName: "Cycle Merchant",
		BaseURL:      upstreamBaseURL,
		APIKey:       "sk-cycle-upstream",
		Platform:     service.PlatformOpenAI,
	})
	require.NoError(t, err)
	require.NotNil(t, merchant)

	applicant, err = userRepo.GetByID(ctx, applicant.ID)
	require.NoError(t, err)
	require.Equal(t, service.RoleMerchant, applicant.Role)
	require.NotNil(t, applicant.MerchantID)
	require.Equal(t, merchant.ID, *applicant.MerchantID)

	groups, err := groupRepo.ListActive(ctx)
	require.NoError(t, err)
	var group *service.Group
	for i := range groups {
		if groups[i].Name == "Cycle Merchant" {
			group = &groups[i]
			break
		}
	}
	require.NotNil(t, group, "apply must create the merchant group")
	require.NotNil(t, group.MerchantID)
	require.Equal(t, merchant.ID, *group.MerchantID)

	// 货架只展示有模型的商户分组。
	shelfSvc := service.NewModelPlazaService(channelRepo, groupRepo, nil, nil, nil)
	shelf, err := shelfSvc.ListGroups(ctx)
	require.NoError(t, err)
	var shelfGroup *service.PlazaGroup
	for i := range shelf {
		if shelf[i].ID == group.ID {
			shelfGroup = &shelf[i]
			break
		}
	}
	require.NotNil(t, shelfGroup, "merchant group must be visible on the shelf right after apply")
	require.NotNil(t, shelfGroup.MerchantID)
	require.Equal(t, merchant.ID, *shelfGroup.MerchantID)
	require.NotEmpty(t, shelfGroup.Models)

	// 固定 12.5% 抽成，便于对 47 条成功请求做精确金额断言。
	require.NoError(t, merchantSvc.SetCommissionRate(ctx, merchant.ID, cycleCommission))
	merchant, err = merchantSvc.Get(ctx, merchant.ID)
	require.NoError(t, err)
	require.InDelta(t, cycleCommission, merchant.CommissionRate, 1e-12)

	account, err := onboardingAccountRepo.GetMerchantGroupAccount(ctx, merchant.ID, group.ID)
	require.NoError(t, err)
	require.NotNil(t, account)
	// 商户上游只实现 OpenAI 兼容 CC，没有 /v1/responses；网关必须直转 CC。
	require.NoError(t, client.Account.UpdateOneID(account.ID).
		SetExtra(map[string]any{"openai_responses_supported": false}).
		Exec(ctx))
	account.Extra = map[string]any{"openai_responses_supported": false}

	// ---- 消费者：余额充足，绑在该商户分组上的 key ----
	consumer := &service.User{
		Email:        "cycle-consumer@example.com",
		PasswordHash: "test-password-hash",
		Role:         service.RoleUser,
		Status:       service.StatusActive,
		Balance:      1000,
		Concurrency:  5,
	}
	require.NoError(t, userRepo.Create(ctx, consumer))

	key := &service.APIKey{
		UserID:  consumer.ID,
		Key:     "sk-cycle-consumer",
		Name:    "cycle-consumer",
		Status:  service.StatusActive,
		GroupID: &group.ID,
	}
	require.NoError(t, apiKeyRepo.Create(ctx, key))

	apiKeySvc := service.NewAPIKeyService(apiKeyRepo, userRepo, groupRepo, userSubRepo, userGroupRateRepo, nil, cfg)
	loadedKey, authenticatedUser, err := apiKeySvc.ValidateKey(ctx, key.Key)
	require.NoError(t, err)
	require.NotNil(t, loadedKey.Group)
	require.NotNil(t, loadedKey.Group.MerchantID)
	require.Equal(t, merchant.ID, *loadedKey.Group.MerchantID)
	require.Equal(t, consumer.ID, authenticatedUser.ID)

	gateway := service.NewOpenAIGatewayService(
		accountRepo,
		usageLogRepo,
		usageBillingRepo,
		userRepo,
		userSubRepo,
		userGroupRateRepo,
		nil,
		cfg,
		nil,
		nil,
		service.NewBillingService(cfg, nil),
		nil,
		nil,
		&cycleHTTPUpstream{client: httpClient},
		&service.DeferredService{},
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		merchantRepo,
	)

	metricsSvc := service.NewMetricsService(
		repository.NewRequestMetricRepository(client),
		service.MetricsOptions{Buffer: 128, BatchSize: 64, FlushInterval: 10 * time.Millisecond},
	)
	metricsSvc.Start()

	gin.SetMode(gin.TestMode)
	metricTimes := make([]time.Time, 0, cycleRequestCount)
	usageTimes := make([]time.Time, 0, cycleSuccessCount)

	issue := func(at time.Time, stream bool, fail bool) {
		t.Helper()
		content := "ping"
		if fail {
			content = cycleFailMarker
		}
		requestBody, err := json.Marshal(map[string]any{
			"model":    cycleUpstreamModel,
			"stream":   stream,
			"messages": []map[string]string{{"role": "user", "content": content}},
		})
		require.NoError(t, err)

		recorder := httptest.NewRecorder()
		ginCtx, _ := gin.CreateTestContext(recorder)
		httpReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(requestBody))
		httpReq.Header.Set("Content-Type", "application/json")
		ginCtx.Request = httpReq

		result, forwardErr := gateway.ForwardAsChatCompletions(ctx, ginCtx, account, requestBody, "", "")
		if fail {
			require.Error(t, forwardErr, "synthetic 502 must surface as a gateway error")
			var failover *service.UpstreamFailoverError
			require.ErrorAs(t, forwardErr, &failover, "gateway must classify the 502 as an upstream failover")
			require.Equal(t, http.StatusBadGateway, failover.StatusCode)
			metricsSvc.Record(ctx, service.RequestMetric{
				RequestID:     fmt.Sprintf("cycle-%d", len(metricTimes)),
				UserID:        authenticatedUser.ID,
				APIKeyID:      loadedKey.ID,
				GroupID:       group.ID,
				MerchantID:    &merchant.ID,
				Model:         cycleUpstreamModel,
				StatusCode:    failover.StatusCode,
				UpstreamError: service.ClassifyUpstreamFailure(failover.StatusCode),
			})
			metricTimes = append(metricTimes, at)
			return
		}

		require.NoError(t, forwardErr)
		require.NotNil(t, result)
		require.Equal(t, cycleInputTokens, result.Usage.InputTokens)
		require.Equal(t, cycleOutputTokens, result.Usage.OutputTokens)
		require.NoError(t, gateway.RecordUsage(ctx, &service.OpenAIRecordUsageInput{
			Result:           result,
			APIKey:           loadedKey,
			User:             loadedKey.User,
			Account:          account,
			InboundEndpoint:  "/v1/chat/completions",
			UpstreamEndpoint: "/v1/chat/completions",
			QuotaPlatform:    service.PlatformOpenAI,
		}))

		firstTokenMS := 0
		if result.FirstTokenMs != nil {
			firstTokenMS = *result.FirstTokenMs
		}
		metricsSvc.Record(ctx, service.RequestMetric{
			RequestID:           fmt.Sprintf("cycle-%d", len(metricTimes)),
			UserID:              authenticatedUser.ID,
			APIKeyID:            loadedKey.ID,
			GroupID:             group.ID,
			MerchantID:          &merchant.ID,
			Model:               cycleUpstreamModel,
			StatusCode:          http.StatusOK,
			DurationMS:          int(result.Duration.Milliseconds()),
			FirstTokenMS:        firstTokenMS,
			InputTokens:         result.Usage.InputTokens,
			OutputTokens:        result.Usage.OutputTokens,
			CacheReadTokens:     result.Usage.CacheReadInputTokens,
			CacheCreationTokens: result.Usage.CacheCreationInputTokens,
		})
		metricTimes = append(metricTimes, at)
		usageTimes = append(usageTimes, at)
	}

	// Day1 / Day2 各 15 条，流式与非流式混发；Day3 的 20 条决定降级判定。
	for i := 0; i < 15; i++ {
		issue(day1.Add(time.Duration(i)*time.Minute), i%2 == 0, false)
	}
	for i := 0; i < 15; i++ {
		issue(day2.Add(time.Duration(i)*time.Minute), i%2 == 1, false)
	}
	for i := 0; i < 17; i++ {
		issue(day3Traffic, i%3 == 0, false)
	}
	// 3 次上游 502 放在最后：网关的错误处理可能暂时禁用账号，不能影响前面的成功请求。
	for i := 0; i < cycleFailureCount; i++ {
		issue(day3Traffic, false, true)
	}

	require.NoError(t, metricsSvc.Close(ctx))
	require.Equal(t, int64(cycleRequestCount), metricsSvc.Inserted(), "every request metric must be persisted")
	require.Zero(t, metricsSvc.Failed())
	require.Len(t, metricTimes, cycleRequestCount)
	require.Len(t, usageTimes, cycleSuccessCount)

	upstreamMu.Lock()
	require.Equal(t, cycleRequestCount+2, gatewayPosts, "probe uses 2 POSTs; the cycle issues 50 gateway requests")
	upstreamMu.Unlock()

	// request_metrics 与 usage_logs 的 created_at 由仓储在真实时刻写入；
	// 这里按注入的 clock 把行归位到对应账期。UPDATE 只改时间，不改金额。
	cycleAssignRequestMetricTimes(t, ctx, db, metricTimes)
	cycleAssignUsageLogTimes(t, ctx, db, merchant.ID, usageTimes)

	// ---- Day3：1h 窗口 20 条、3 个上游失败 = 15% > 10%，应标记降级 ----
	clock.Set(day3Health)
	aggregator := service.NewMetricsAggregator(groupMetricRepo)
	service.SetMetricsAggregatorClockForTest(aggregator, clock.Now)

	health, err := aggregator.Health(ctx, group.ID)
	require.NoError(t, err)
	require.True(t, health.Degraded, "20 samples with 3 upstream failures must be degraded")
	require.Equal(t, service.DegradedReasonUpstreamFailureRate, health.Reason)
	require.Equal(t, 20, health.SampleSize)

	for _, hour := range []time.Time{
		day1.Truncate(time.Hour),
		day2.Truncate(time.Hour),
		day3Traffic.Truncate(time.Hour),
	} {
		require.NoError(t, aggregator.AggregateHour(ctx, hour))
	}
	hourly, err := groupMetricRepo.ListGroupMetrics(ctx, []int64{group.ID}, weekStart, weekEnd)
	require.NoError(t, err)
	require.Len(t, hourly, 3)
	var hourlyRequests, hourlyFailures int
	for _, row := range hourly {
		hourlyRequests += row.Requests
		hourlyFailures += row.UpstreamFailures
		if row.Hour.Equal(day3Traffic.Truncate(time.Hour)) {
			require.Equal(t, 20, row.Requests)
			require.Equal(t, cycleFailureCount, row.UpstreamFailures)
		}
	}
	require.Equal(t, cycleRequestCount, hourlyRequests)
	require.Equal(t, cycleFailureCount, hourlyFailures)

	// ---- Day8：生成整周结算单 ----
	clock.Set(day8)
	healthAtSettlement, err := aggregator.Health(ctx, group.ID)
	require.NoError(t, err)
	// 降级是“最近 1 小时/15 分钟”的实时信号，不是历史标签。Day8 距最后一条
	// 请求已 5 天，窗口内没有样本，精确的预期状态就是：样本 0、未降级。
	require.False(t, healthAtSettlement.Degraded)
	require.Equal(t, 0, healthAtSettlement.SampleSize)
	t.Logf("[cycle] Day3 degraded=%v reason=%q sample=%d", health.Degraded, health.Reason, health.SampleSize)
	t.Logf("[cycle] Day8 degraded=%v reason=%q sample=%d (live 1h window has no traffic)", healthAtSettlement.Degraded, healthAtSettlement.Reason, healthAtSettlement.SampleSize)

	settlementSvc := service.NewSettlementService(settlementRepo, cfg)
	service.SetSettlementServiceClockForTest(settlementSvc, clock.Now)
	settlement, err := settlementSvc.Generate(ctx, merchant.ID, weekStart, weekEnd)
	require.NoError(t, err)
	require.NotNil(t, settlement)
	require.Equal(t, service.SettlementStatusPending, settlement.Status)
	require.True(t, settlement.PeriodStart.Equal(weekStart))
	require.True(t, settlement.PeriodEnd.Equal(weekEnd))

	wantGross := cycleSuccessCount * cycleCostPerCall
	wantCommission := wantGross * cycleCommission
	wantNet := wantGross - wantCommission
	require.InDelta(t, wantGross, settlement.GrossCost, 1e-6, "gross must be 47 successful requests × $4")
	require.InDelta(t, wantCommission, settlement.Commission, 1e-6, "commission must use the 12.5%% write-time snapshot")
	require.InDelta(t, wantNet, settlement.NetPayable, 1e-6)

	ops := service.NewSettlementOps(settlementOpsRepo)
	service.SetSettlementOpsClockForTest(ops, clock.Now)
	items, err := ops.Explore(ctx, settlement.ID)
	require.NoError(t, err)
	require.Len(t, items, cycleSuccessCount, "failed upstream requests never produce billable usage")
	var itemCost, itemCommission float64
	for _, item := range items {
		itemCost += item.Cost
		itemCommission += item.Commission
	}
	require.InDelta(t, wantGross, itemCost, 1e-6)
	require.InDelta(t, wantCommission, itemCommission, 1e-6)

	// settlements.created_at / dispute_deadline 由数据库 NOW() 生成。为了保持
	// 时间旅行的确定性，先把它们归位到 Day8，并先验证生产逻辑确实给出 +7 天窗口。
	cycleNormalizeSettlementTimes(t, ctx, db, settlement.ID, day8)

	// 结算单在 Day8 由运营确认后才入账；pending 从未入账，申诉时没有余额可冻结。
	require.NoError(t, ops.Confirm(ctx, settlement.ID, "ops@example.com"))
	confirmed, err := settlementOpsRepo.GetSettlement(ctx, settlement.ID)
	require.NoError(t, err)
	require.Equal(t, service.SettlementStatusConfirmed, confirmed.Status)
	require.NotNil(t, confirmed.ConfirmedAt)
	require.WithinDuration(t, day8, *confirmed.ConfirmedAt, time.Second)

	balance, frozen := cycleMerchantBalance(t, ctx, db, merchant.ID)
	require.InDelta(t, wantNet, balance, 1e-9)
	require.Zero(t, frozen)

	ledgerAfterConfirm := cycleLedgerEntries(t, ctx, db, merchant.ID)
	require.Len(t, ledgerAfterConfirm, 1)
	require.Equal(t, "settle", ledgerAfterConfirm[0].Type)
	require.Equal(t, "settlement", ledgerAfterConfirm[0].RefType)
	require.Equal(t, settlement.ID, ledgerAfterConfirm[0].RefID)
	require.InDelta(t, wantNet, ledgerAfterConfirm[0].Amount, 1e-9)

	// ---- Day9：商户申诉 → 结算冻结；运营按 -3 调账 ----
	clock.Set(day9)
	disputeSvc := service.NewDisputeService(disputeRepo)
	service.SetDisputeServiceClockForTest(disputeSvc, clock.Now)

	dispute, err := disputeSvc.Open(ctx, service.OpenDisputeInput{
		OwnerType:  service.DisputeOwnerMerchant,
		OwnerID:    merchant.ID,
		TargetType: service.DisputeTargetSettlement,
		TargetID:   settlement.ID,
		Amount:     3,
		Reason:     "3 requests were billed even though the upstream returned 502",
	})
	require.NoError(t, err)
	require.NotNil(t, dispute)
	require.Equal(t, service.DisputeStatusOpen, dispute.Status)

	disputed, err := settlementOpsRepo.GetSettlement(ctx, settlement.ID)
	require.NoError(t, err)
	require.Equal(t, service.SettlementStatusDisputed, disputed.Status)

	balance, frozen = cycleMerchantBalance(t, ctx, db, merchant.ID)
	require.Zero(t, balance, "confirmed settlement funds must move out of available balance")
	require.InDelta(t, wantNet, frozen, 1e-9, "confirmed settlement net must be frozen while disputed")

	clock.Set(day9.Add(time.Hour))
	resolved, err := disputeSvc.Resolve(ctx, dispute.ID, service.ResolveInput{
		Decision:     service.DisputeDecisionAdjust,
		AdjustAmount: -3,
		Note:         "confirmed 3 invalid 502 charges",
		By:           "ops@example.com",
	})
	require.NoError(t, err)
	require.Equal(t, service.DisputeStatusResolved, resolved.Status)
	require.Equal(t, service.DisputeDecisionAdjust, resolved.Decision)
	require.InDelta(t, -3, resolved.AdjustAmount, 1e-12)

	confirmedAgain, err := settlementOpsRepo.GetSettlement(ctx, settlement.ID)
	require.NoError(t, err)
	require.Equal(t, service.SettlementStatusConfirmed, confirmedAgain.Status)

	balance, frozen = cycleMerchantBalance(t, ctx, db, merchant.ID)
	require.InDelta(t, wantNet-3, balance, 1e-9, "adjustment must reduce available balance by exactly $3")
	require.Zero(t, frozen, "dispute freeze must be fully released by the adjustment")

	ledgerAfterResolve := cycleLedgerEntries(t, ctx, db, merchant.ID)
	var adjustEntry *cycleLedgerEntry
	for i := range ledgerAfterResolve {
		if ledgerAfterResolve[i].Type == "adjust" {
			adjustEntry = &ledgerAfterResolve[i]
		}
	}
	require.NotNil(t, adjustEntry, "adjust decision must write a ledger entry")
	require.Equal(t, "dispute", adjustEntry.RefType, "adjust must reference its dispute")
	require.Equal(t, dispute.ID, adjustEntry.RefID)
	require.InDelta(t, -3, adjustEntry.Amount, 1e-12)

	// ---- Day9：全额提现 → 运营审核 → 打款，余额/冻结归零 ----
	clock.Set(day9.Add(2 * time.Hour))
	withdrawalSvc := service.NewWithdrawalService(withdrawalRepo)
	service.SetWithdrawalServiceClockForTest(withdrawalSvc, clock.Now)

	available, err := withdrawalSvc.Available(ctx, merchant.ID)
	require.NoError(t, err)
	require.InDelta(t, wantNet-3, available, 1e-9)

	withdrawal, err := withdrawalSvc.Request(ctx, merchant.ID, available)
	require.NoError(t, err)
	require.NotNil(t, withdrawal)
	require.Equal(t, service.WithdrawalStatusRequested, withdrawal.Status)
	require.InDelta(t, available, withdrawal.Amount, 1e-12)

	balance, frozen = cycleMerchantBalance(t, ctx, db, merchant.ID)
	require.Zero(t, balance)
	require.InDelta(t, available, frozen, 1e-9)

	require.NoError(t, withdrawalSvc.Approve(ctx, withdrawal.ID, "ops@example.com"))
	require.NoError(t, withdrawalSvc.MarkPaid(
		ctx, withdrawal.ID, "proofs/cycle-payout.pdf", strings.Repeat("a", 64), "ops@example.com"))

	var (
		withdrawalStatus string
		paidAt           sql.NullTime
		proofSHA         string
	)
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT status, paid_at, proof_sha256 FROM withdrawals WHERE id = $1`, withdrawal.ID).
		Scan(&withdrawalStatus, &paidAt, &proofSHA))
	require.Equal(t, service.WithdrawalStatusPaid, withdrawalStatus)
	require.True(t, paidAt.Valid)
	require.WithinDuration(t, clock.Now(), paidAt.Time, time.Second)
	require.Equal(t, strings.Repeat("a", 64), proofSHA)

	balance, frozen = cycleMerchantBalance(t, ctx, db, merchant.ID)
	require.Zero(t, balance, "paid withdrawal must leave no available balance")
	require.Zero(t, frozen, "paid withdrawal must release all frozen funds")

	finalLedger := cycleLedgerEntries(t, ctx, db, merchant.ID)
	var ledgerSum float64
	for _, entry := range finalLedger {
		ledgerSum += entry.Amount
	}
	require.InDelta(t, 0, ledgerSum, 1e-9, "settle + hold + release + adjust + withdraw must net to zero")

	consumerAfter, err := userRepo.GetByID(ctx, consumer.ID)
	require.NoError(t, err)
	require.InDelta(t, 1000-wantGross, consumerAfter.Balance, 1e-9,
		"consumer must pay exactly the 47 successful requests × $4")

	t.Logf("[cycle] settlement id=%d gross=%.10f commission=%.10f net=%.10f", settlement.ID, settlement.GrossCost, settlement.Commission, settlement.NetPayable)
	t.Logf("[cycle] dispute id=%d adjust=%.4f ledger_entries=%d", dispute.ID, adjustEntry.Amount, len(finalLedger))
	t.Logf("[cycle] withdrawal id=%d amount=%.10f status=%s balance=%.10f frozen=%.10f ledger_sum=%.10f",
		withdrawal.ID, withdrawal.Amount, withdrawalStatus, balance, frozen, ledgerSum)
}

// cycleAssignRequestMetricTimes 按 id 顺序把指标行归位到注入时钟的时刻。
// MetricsService 的写入路径不带时间参数，异步 worker 的 created_at 是真实时刻。
func cycleAssignRequestMetricTimes(t *testing.T, ctx context.Context, db *sql.DB, times []time.Time) {
	t.Helper()
	ids := cycleIDs(t, ctx, db, `SELECT id FROM request_metrics ORDER BY id`)
	require.Len(t, ids, len(times))
	for i, id := range ids {
		_, err := db.ExecContext(ctx,
			`UPDATE request_metrics SET created_at = $1 WHERE id = $2`, times[i].UTC(), id)
		require.NoError(t, err)
	}
}

// cycleAssignUsageLogTimes 把本周成功请求的用量行归位到对应账期。
func cycleAssignUsageLogTimes(t *testing.T, ctx context.Context, db *sql.DB, merchantID int64, times []time.Time) {
	t.Helper()
	ids := cycleIDs(t, ctx, db,
		`SELECT id FROM usage_logs WHERE merchant_id = $1 ORDER BY id`, merchantID)
	require.Len(t, ids, len(times))
	for i, id := range ids {
		_, err := db.ExecContext(ctx,
			`UPDATE usage_logs SET created_at = $1 WHERE id = $2`, times[i].UTC(), id)
		require.NoError(t, err)
	}
}

// cycleNormalizeSettlementTimes 验证数据库生成的申诉窗口是 created_at + 7d，
// 再把时间归位到注入时钟的 Day8。
func cycleNormalizeSettlementTimes(t *testing.T, ctx context.Context, db *sql.DB, settlementID int64, at time.Time) {
	t.Helper()
	var createdAt, deadline time.Time
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT created_at, dispute_deadline FROM settlements WHERE id = $1`, settlementID).
		Scan(&createdAt, &deadline))
	require.InDelta(t, (7 * 24 * time.Hour).Seconds(), deadline.Sub(createdAt).Seconds(), 1,
		"production must set dispute_deadline = created_at + 7 days")

	_, err := db.ExecContext(ctx, `
UPDATE settlements
SET created_at = $1::timestamptz,
    updated_at = $1::timestamptz,
    dispute_deadline = $1::timestamptz + interval '7 days'
WHERE id = $2`, at.UTC(), settlementID)
	require.NoError(t, err)
}

func cycleIDs(t *testing.T, ctx context.Context, db *sql.DB, query string, args ...any) []int64 {
	t.Helper()
	rows, err := db.QueryContext(ctx, query, args...)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		require.NoError(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	require.NoError(t, rows.Err())
	return ids
}

func cycleMerchantBalance(t *testing.T, ctx context.Context, db *sql.DB, merchantID int64) (balance, frozen float64) {
	t.Helper()
	err := db.QueryRowContext(ctx,
		`SELECT balance, frozen FROM merchant_balance WHERE merchant_id = $1`, merchantID).
		Scan(&balance, &frozen)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0
	}
	require.NoError(t, err)
	return balance, frozen
}

func cycleLedgerEntries(t *testing.T, ctx context.Context, db *sql.DB, merchantID int64) []cycleLedgerEntry {
	t.Helper()
	rows, err := db.QueryContext(ctx, `
SELECT id, type, amount, ref_type, ref_id, created_at
FROM merchant_ledger
WHERE merchant_id = $1
ORDER BY id`, merchantID)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	entries := make([]cycleLedgerEntry, 0)
	for rows.Next() {
		var entry cycleLedgerEntry
		require.NoError(t, rows.Scan(
			&entry.ID, &entry.Type, &entry.Amount, &entry.RefType, &entry.RefID, &entry.CreatedAt))
		entries = append(entries, entry)
	}
	require.NoError(t, rows.Err())
	return entries
}
