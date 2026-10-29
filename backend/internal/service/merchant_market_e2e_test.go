//go:build !unit

// TestMarketE2E 需要真实 PostgreSQL（testcontainers），因此不参与 -tags=unit 的
// 单元测试套件；默认套件与 -tags=integration 下运行，没有 Docker 时自动跳过。
package service_test

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/woolsen/router2api/ent"
	_ "github.com/woolsen/router2api/ent/runtime"
	"github.com/woolsen/router2api/ent/usagelog"
	"github.com/woolsen/router2api/internal/config"
	"github.com/woolsen/router2api/internal/pkg/timezone"
	"github.com/woolsen/router2api/internal/repository"
	"github.com/woolsen/router2api/internal/service"
	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// marketUpstreamRequest 记录 fake 商户上游实际收到的一次请求。
type marketUpstreamRequest struct {
	Path          string
	Authorization string
	Body          string
}

// TestMarketE2E 走查一条真实的商户请求，串起 Task 1–5 的全部产出：
//
//	建商户 → 建分组（归属商户）→ AttachGroup 建上游账号 → 用户/Key
//	→ 网关把 /v1/chat/completions 转发到 fake 商户上游
//	→ 写入 usage_logs（merchant_id/commission_rate 快照）并扣减用户余额
//	→ 按周聚合生成结算单（net = gross × (1 − commission)）。
//
// 断言 3、4 走真实 PostgreSQL（ent + repository），不是桩。
func TestMarketE2E(t *testing.T) {
	ctx := context.Background()
	if err := timezone.Init("UTC"); err != nil {
		t.Fatalf("init timezone: %v", err)
	}
	client, db := marketE2EDatabase(t, ctx)

	// ---- fake 商户上游：固定 usage + 一段 SSE，并记录收到的请求头 ----
	var upstreamMu sync.Mutex
	var upstreamReqs []marketUpstreamRequest
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		upstreamMu.Lock()
		upstreamReqs = append(upstreamReqs, marketUpstreamRequest{
			Path:          r.URL.Path,
			Authorization: r.Header.Get("Authorization"),
			Body:          string(body),
		})
		upstreamMu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("x-request-id", "req-merchant-e2e-1")
		w.WriteHeader(http.StatusOK)
		chunks := []string{
			`data: {"id":"chatcmpl-e2e","object":"chat.completion.chunk","model":"gpt-5.4","choices":[{"index":0,"delta":{"role":"assistant","content":"hello from merchant"},"finish_reason":null}]}`,
			``,
			`data: {"id":"chatcmpl-e2e","object":"chat.completion.chunk","model":"gpt-5.4","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000,"completion_tokens":500,"total_tokens":1500}}`,
			``,
			`data: [DONE]`,
			``,
		}
		_, _ = io.WriteString(w, strings.Join(chunks, "\n"))
	}))
	defer upstream.Close()

	// ---- 真实仓储 ----
	merchantRepo := repository.NewMerchantRepository(client)
	groupRepo := repository.NewGroupRepository(client, db)
	accountRepo := repository.NewAccountRepository(client, db, nil)
	onboardingAccountRepo, ok := accountRepo.(service.MerchantOnboardingAccountRepository)
	require.True(t, ok, "account repository must expose the merchant onboarding capabilities")
	userRepo := repository.NewUserRepository(client, db)
	apiKeyRepo := repository.NewAPIKeyRepository(client, db)
	usageLogRepo := repository.NewUsageLogRepository(client, db)
	usageBillingRepo := repository.NewUsageBillingRepository(client, db)
	userSubRepo := repository.NewUserSubscriptionRepository(client)
	userGroupRateRepo := repository.NewUserGroupRateRepository(db)
	settlementRepo := repository.NewSettlementRepository(client, db)

	merchantSvc := service.NewMerchantService(merchantRepo)
	onboardingSvc := service.NewMerchantOnboardingService(merchantSvc, onboardingAccountRepo, groupRepo)

	// 网关/认证服务共用配置：标准计费模式；fake 上游是 http://127.0.0.1，
	// 生产里接内网中转站同样需要管理员显式放开 allow_insecure_http。
	cfg := &config.Config{RunMode: config.RunModeStandard}
	cfg.Default.RateMultiplier = 1
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true

	// 1) 建商户（抽成故意取非默认的 12.5%），绑一个平台为 openai 的市场分组。
	// 用非默认值是关键：若实现把所有商户都按 MerchantDefaultCommissionRate(8%) 计费，
	// 下面 usage_logs/结算的断言必须失败。
	const snapshotCommission = 0.125
	commission := snapshotCommission
	merchant, err := merchantSvc.Create(ctx, service.CreateMerchantInput{
		Name:           "E2E Merchant",
		Contact:        "e2e@merchant.test",
		CommissionRate: &commission,
	})
	require.NoError(t, err)

	group := &service.Group{
		Name:             "merchant-e2e-openai",
		Platform:         service.PlatformOpenAI,
		Status:           service.StatusActive,
		SubscriptionType: service.SubscriptionTypeStandard,
		RateMultiplier:   1,
		MerchantID:       &merchant.ID,
	}
	require.NoError(t, groupRepo.Create(ctx, group))

	// 2) AttachGroup：为分组创建唯一上游账号，凭证指向 fake 上游。
	account, err := onboardingSvc.AttachGroup(ctx, service.AttachGroupInput{
		MerchantID: merchant.ID,
		GroupID:    group.ID,
		BaseURL:    upstream.URL,
		APIKey:     "sk-up",
		Platform:   service.PlatformOpenAI,
	})
	require.NoError(t, err)
	require.NotNil(t, account)
	require.Equal(t, service.AccountTypeAPIKey, account.Type)
	require.Equal(t, "sk-up", account.GetCredential("api_key"))
	require.Equal(t, upstream.URL, account.GetCredential("base_url"))

	// 商户上游是 OpenAI 兼容中转站：没有 /v1/responses，只能直转 CC。
	// 这是管理员在账号上做的探测配置，测试按真实配置写入。
	require.NoError(t, client.Account.UpdateOneID(account.ID).
		SetExtra(map[string]any{"openai_responses_supported": false}).
		Exec(ctx))
	account.Extra = map[string]any{"openai_responses_supported": false}

	// 3) 用户（余额 100）与绑在该分组上的 API Key。
	user := &service.User{
		Email:        fmt.Sprintf("market-e2e-%d@example.com", time.Now().UnixNano()),
		PasswordHash: "test-password-hash",
		Role:         service.RoleUser,
		Status:       service.StatusActive,
		Balance:      100,
		Concurrency:  5,
	}
	require.NoError(t, userRepo.Create(ctx, user))

	key := &service.APIKey{
		UserID:  user.ID,
		Key:     fmt.Sprintf("sk-e2e-%d", time.Now().UnixNano()),
		Name:    "market-e2e",
		Status:  service.StatusActive,
		GroupID: &group.ID,
	}
	require.NoError(t, apiKeyRepo.Create(ctx, key))

	// 4) 认证链路：用明文 key 经 APIKeyService → GetByKeyForAuth → 认证快照往返，
	// 拿到 apiKey 及其分组（merchant_id 必须从 groups 表真实带出）。
	// 这一段替代手工取 key，证明 key → 组 → merchant_id 的认证投影没有漏列。
	apiKeySvc := service.NewAPIKeyService(apiKeyRepo, userRepo, groupRepo, userSubRepo, userGroupRateRepo, nil, cfg)
	loadedKey, authenticatedUser, err := apiKeySvc.ValidateKey(ctx, key.Key)
	require.NoError(t, err)
	require.NotNil(t, loadedKey)
	require.NotNil(t, authenticatedUser)
	require.NotNil(t, loadedKey.Group)
	require.NotNil(t, loadedKey.Group.MerchantID, "认证快照必须带出分组所属商户")
	require.Equal(t, merchant.ID, *loadedKey.Group.MerchantID)
	require.Equal(t, user.ID, authenticatedUser.ID)
	require.Equal(t, group.ID, *loadedKey.GroupID)

	// ---- 网关：真实仓储 + 固定回退价格表（gpt-5.4 = $2.5/$15 per MTok）----
	gateway := service.NewOpenAIGatewayService(
		accountRepo,
		usageLogRepo,
		usageBillingRepo,
		userRepo,
		userSubRepo,
		userGroupRateRepo,
		nil, // gateway cache：本测试路径不读写缓存
		cfg,
		nil, // scheduler snapshot：直连转发不需要选号
		nil, // concurrency service
		service.NewBillingService(cfg, nil),
		nil, // rate limit service
		nil, // billing cache
		repository.NewHTTPUpstream(cfg),
		&service.DeferredService{},
		nil, // openai token provider（api key 账号不需要）
		nil, // grok token provider
		nil, // pricing resolver
		nil, // channel service
		nil, // balance notify
		nil, // setting service
		nil, // user platform quota repo
		merchantRepo,
	)

	// 5) 发一条 /v1/chat/completions（stream），确认上游收到商户凭证。
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	requestBody := []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"ping"}],"stream":true}`)
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(requestBody))
	httpReq.Header.Set("Content-Type", "application/json")
	ginCtx.Request = httpReq

	forwardResult, err := gateway.ForwardAsChatCompletions(ctx, ginCtx, account, requestBody, "", "")
	require.NoError(t, err)
	require.NotNil(t, forwardResult)
	require.Equal(t, 1000, forwardResult.Usage.InputTokens)
	require.Equal(t, 500, forwardResult.Usage.OutputTokens)

	upstreamMu.Lock()
	require.Len(t, upstreamReqs, 1, "网关必须且只能向商户上游转发一次")
	got := upstreamReqs[0]
	upstreamMu.Unlock()
	require.Equal(t, "/v1/chat/completions", got.Path)
	require.Equal(t, "Bearer sk-up", got.Authorization, "上游必须收到商户 api key")
	require.Contains(t, got.Body, `"gpt-5.4"`)
	t.Logf("[market-e2e] upstream: POST %s Authorization=%q", got.Path, got.Authorization)

	// 6) 记录用量：真实写入 usage_logs 并扣减余额。
	require.NoError(t, gateway.RecordUsage(ctx, &service.OpenAIRecordUsageInput{
		Result:           forwardResult,
		APIKey:           loadedKey,
		User:             loadedKey.User,
		Account:          account,
		InboundEndpoint:  "/v1/chat/completions",
		UpstreamEndpoint: "/v1/chat/completions",
		QuotaPlatform:    service.PlatformOpenAI,
	}))

	usageLog, err := client.UsageLog.Query().
		Where(usagelog.APIKeyIDEQ(loadedKey.ID)).
		Only(ctx)
	require.NoError(t, err)
	// token 与模型都由 fake 上游固定：1000 × $2.5/MTok + 500 × $15/MTok = $0.01。
	// 断言具体金额，避免「价格表返回 0 也通过」。
	const wantActualCost = 0.01
	require.InDelta(t, wantActualCost, usageLog.ActualCost, 1e-9,
		"actual_cost 应为 %.8f，实际 %.8f（input_tokens=%d output_tokens=%d model=%s）",
		wantActualCost, usageLog.ActualCost, usageLog.InputTokens, usageLog.OutputTokens, usageLog.Model)
	require.NotNil(t, usageLog.MerchantID)
	require.Equal(t, merchant.ID, *usageLog.MerchantID, "usage_logs 必须快照 merchant_id")
	require.NotNil(t, usageLog.CommissionRate)
	require.InDelta(t, snapshotCommission, *usageLog.CommissionRate, 1e-12,
		"usage_logs 必须按写入当时的商户费率快照 commission_rate（非默认 8%%）")

	reloadedUser, err := userRepo.GetByID(ctx, user.ID)
	require.NoError(t, err)
	require.InDelta(t, 100-usageLog.ActualCost, reloadedUser.Balance, 1e-9,
		"用户余额必须精确减少 actual_cost")
	t.Logf("[market-e2e] usage_log: id=%d actual_cost=%.8f merchant_id=%d commission_rate=%.4f",
		usageLog.ID, usageLog.ActualCost, *usageLog.MerchantID, *usageLog.CommissionRate)
	t.Logf("[market-e2e] balance: %.8f -> %.8f (delta=%.8f)", 100.0, reloadedUser.Balance, 100.0-reloadedUser.Balance)

	// 写时快照的关键证明：写完之后把商户费率改成 30%，结算仍必须按 12.5% 计算。
	const newCommission = 0.30
	require.NoError(t, merchantSvc.SetCommissionRate(ctx, merchant.ID, newCommission))
	reloadedMerchant, err := merchantSvc.Get(ctx, merchant.ID)
	require.NoError(t, err)
	require.InDelta(t, newCommission, reloadedMerchant.CommissionRate, 1e-12,
		"商户当前费率必须已改为 %.2f", newCommission)

	// 7) 结算：把用量挪进一个已经封账的自然周，按周生成结算单。
	weekEnd := startOfUTCWeek(time.Now()).AddDate(0, 0, -7)
	weekStart := weekEnd.AddDate(0, 0, -7)
	// usage_logs.created_at 在 ent 模型里不可变，按集成测试惯例用原生 SQL 把这条
	// 用量挪进历史账期，模拟一周后结算。
	_, err = db.ExecContext(ctx,
		`UPDATE usage_logs SET created_at = $1 WHERE id = $2`,
		weekStart.Add(72*time.Hour), usageLog.ID)
	require.NoError(t, err)

	settlementSvc := service.NewSettlementService(settlementRepo, cfg)
	settlement, err := settlementSvc.Generate(ctx, merchant.ID, weekStart, weekEnd)
	require.NoError(t, err)
	require.NotNil(t, settlement)
	require.Equal(t, service.SettlementStatusPending, settlement.Status)
	require.InDelta(t, usageLog.ActualCost, settlement.GrossCost, 1e-9)
	require.InDelta(t, settlement.GrossCost*snapshotCommission, settlement.Commission, 1e-9,
		"结算必须按 usage_logs 的 12.5%% 快照抽成，而不是商户当前费率 %.2f", newCommission)
	require.InDelta(t, settlement.GrossCost*(1-snapshotCommission), settlement.NetPayable, 1e-9,
		"net = gross × (1 - 写入时快照费率)")
	t.Logf("[market-e2e] settlement: id=%d period=[%s,%s) gross=%.8f commission=%.8f net=%.8f",
		settlement.ID,
		weekStart.Format(time.RFC3339), weekEnd.Format(time.RFC3339),
		settlement.GrossCost, settlement.Commission, settlement.NetPayable)

	// 重跑同账期必须命中同一张结算单（ON CONFLICT 幂等）。
	again, err := settlementSvc.Generate(ctx, merchant.ID, weekStart, weekEnd)
	require.NoError(t, err)
	require.Equal(t, settlement.ID, again.ID)
	require.InDelta(t, settlement.NetPayable, again.NetPayable, 1e-12)
}

func startOfUTCWeek(t time.Time) time.Time {
	t = t.UTC()
	day := (int(t.Weekday()) + 6) % 7 // Monday = 0
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -day)
}

// marketE2EDatabase 启动一次性 PostgreSQL 容器并应用仓库迁移，
// 复用 repository 集成测试同一套迁移与 ent 模型。
func marketE2EDatabase(t *testing.T, ctx context.Context) (*dbent.Client, *sql.DB) {
	t.Helper()

	if !marketE2EDockerAvailable(ctx) {
		if os.Getenv("CI") != "" {
			t.Fatal("docker is not available (CI=true); market e2e needs a real database")
		}
		t.Skip("docker is not available; start Docker to run TestMarketE2E")
	}

	pgContainer, err := tcpostgres.Run(
		ctx,
		"postgres:18.1-alpine3.23",
		tcpostgres.WithDatabase("sub2api_market_e2e"),
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword("postgres"),
		tcpostgres.BasicWaitStrategies(),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = pgContainer.Terminate(context.Background()) })

	dsn, err := pgContainer.ConnectionString(ctx, "sslmode=disable", "TimeZone=UTC")
	require.NoError(t, err)

	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	require.NoError(t, db.PingContext(ctx))
	require.NoError(t, repository.ApplyMigrations(ctx, db))

	driver := entsql.OpenDB(dialect.Postgres, db)
	client := dbent.NewClient(dbent.Driver(driver))
	t.Cleanup(func() { _ = client.Close() })

	return client, db
}

func marketE2EDockerAvailable(ctx context.Context) bool {
	cmd := exec.CommandContext(ctx, "docker", "info")
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run() == nil
}
