package service

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/woolsen/router2api/internal/config"
)

// merchantSnapshotUsageLogRepo captures the usage row handed to the repository.
// 只实现 Create，未实现 CreateBestEffort，因此 writeUsageLogBestEffort 走同步路径。
type merchantSnapshotUsageLogRepo struct {
	UsageLogRepository
	lastLog *UsageLog
}

func (r *merchantSnapshotUsageLogRepo) Create(_ context.Context, log *UsageLog) (bool, error) {
	r.lastLog = log
	return true, nil
}

type merchantSnapshotUserRepo struct {
	UserRepository
}

// merchantSnapshotMerchantRepo 只实现 Get；其余方法留给嵌入的接口。
type merchantSnapshotMerchantRepo struct {
	MerchantRepository
	merchants map[int64]*Merchant
	calls     int
}

func (r *merchantSnapshotMerchantRepo) Get(_ context.Context, id int64) (*Merchant, error) {
	r.calls++
	m, ok := r.merchants[id]
	if !ok {
		return nil, ErrMerchantNotFound
	}
	return m, nil
}

func newMerchantSnapshotGatewayServiceForTest(usageRepo UsageLogRepository, merchantRepo MerchantRepository) *GatewayService {
	cfg := &config.Config{}
	cfg.Default.RateMultiplier = 1.1
	cfg.RunMode = config.RunModeSimple
	return NewGatewayService(
		nil,
		nil,
		usageRepo,
		nil,
		&merchantSnapshotUserRepo{},
		nil,
		nil,
		nil,
		cfg,
		nil,
		nil,
		NewBillingService(cfg, nil),
		nil,
		&BillingCacheService{},
		nil,
		nil,
		&DeferredService{},
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil, // userPlatformQuotaRepo
		merchantRepo,
	)
}

func captureUsageLogForGroup(t *testing.T, group *Group) (*UsageLog, *merchantSnapshotMerchantRepo) {
	t.Helper()
	usageRepo := &merchantSnapshotUsageLogRepo{}
	merchantRepo := &merchantSnapshotMerchantRepo{
		merchants: map[int64]*Merchant{
			1: {ID: 1, Name: "上游一号", Status: MerchantStatusActive, CommissionRate: 0.08},
		},
	}
	svc := newMerchantSnapshotGatewayServiceForTest(usageRepo, merchantRepo)

	err := svc.RecordUsage(context.Background(), &RecordUsageInput{
		Result: &ForwardResult{
			RequestID: "merchant-snapshot-request",
			Model:     "claude-sonnet-4",
			Usage:     ClaudeUsage{InputTokens: 120, OutputTokens: 30},
			Duration:  time.Second,
		},
		APIKey:  &APIKey{ID: 11, Group: group},
		User:    &User{ID: 21},
		Account: &Account{ID: 31},
	})
	if err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	if usageRepo.lastLog == nil {
		t.Fatal("usage log was not written")
	}
	return usageRepo.lastLog, merchantRepo
}

func captureUsageLog(t *testing.T) *UsageLog {
	t.Helper()
	merchantID := int64(1)
	log, _ := captureUsageLogForGroup(t, &Group{ID: 8, MerchantID: &merchantID, RateMultiplier: 1})
	return log
}

func TestUsageLogSnapshotsMerchantAndCommission(t *testing.T) {
	log := captureUsageLog(t)
	if log.MerchantID == nil || *log.MerchantID != 1 {
		t.Fatal("merchant_id not snapshotted")
	}
	if log.CommissionRate == nil {
		t.Fatal("commission_rate not snapshotted")
	}
	if math.Abs(*log.CommissionRate-0.08) > 1e-9 {
		t.Fatalf("want 0.08, got %v", *log.CommissionRate)
	}
}

func TestUsageLogMerchantNilForSelfOperatedGroup(t *testing.T) {
	// 平台自营分组：没有 merchant_id，也不应去查商户。
	log, merchantRepo := captureUsageLogForGroup(t, &Group{ID: 9, RateMultiplier: 1})
	if log.MerchantID != nil {
		t.Fatal("self-operated usage must not carry merchant_id")
	}
	if log.CommissionRate != nil {
		t.Fatal("self-operated usage must not carry commission")
	}
	if merchantRepo.calls != 0 {
		t.Fatalf("self-operated group must not query merchants, got %d calls", merchantRepo.calls)
	}
}

func TestUsageLogMerchantKeptWhenCommissionUnknown(t *testing.T) {
	// 商户查不到时仍然保留 merchant_id（这一行必须记在该商户名下），
	// commission_rate 留 NULL 交给补结，绝不能写成 0 抽成（平台白送全额）。
	missingMerchantID := int64(404)
	log, _ := captureUsageLogForGroup(t, &Group{ID: 9, MerchantID: &missingMerchantID, RateMultiplier: 1})
	if log.MerchantID == nil || *log.MerchantID != missingMerchantID {
		t.Fatal("merchant_id must be snapshotted even when the merchant lookup fails")
	}
	if log.CommissionRate != nil {
		t.Fatalf("unknown commission must stay NULL, got %v", *log.CommissionRate)
	}
}

// OpenAI 计费主干是另一处独立组装 UsageLog 的代码，必须同样带上商户快照。
func TestUsageLogSnapshotsMerchantOnOpenAIPath(t *testing.T) {
	usageRepo := &merchantSnapshotUsageLogRepo{}
	merchantRepo := &merchantSnapshotMerchantRepo{
		merchants: map[int64]*Merchant{
			1: {ID: 1, Name: "上游一号", Status: MerchantStatusActive, CommissionRate: 0.08},
		},
	}
	cfg := &config.Config{}
	cfg.Default.RateMultiplier = 1.1
	cfg.RunMode = config.RunModeSimple
	svc := NewOpenAIGatewayService(
		nil,
		usageRepo,
		nil,
		&merchantSnapshotUserRepo{},
		nil,
		nil,
		nil,
		cfg,
		nil,
		nil,
		NewBillingService(cfg, nil),
		nil,
		&BillingCacheService{},
		nil,
		&DeferredService{},
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil, // userPlatformQuotaRepo
		merchantRepo,
	)
	merchantID := int64(1)
	err := svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
		Result: &OpenAIForwardResult{
			RequestID: "merchant-snapshot-openai",
			Model:     "gpt-5.1",
			Usage:     OpenAIUsage{InputTokens: 100, OutputTokens: 20},
		},
		APIKey:  &APIKey{ID: 12, Group: &Group{ID: 8, MerchantID: &merchantID, RateMultiplier: 1}},
		User:    &User{ID: 22},
		Account: &Account{ID: 32},
	})
	if err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	if usageRepo.lastLog == nil {
		t.Fatal("usage log was not written")
	}
	if usageRepo.lastLog.MerchantID == nil || *usageRepo.lastLog.MerchantID != 1 {
		t.Fatal("merchant_id not snapshotted on openai path")
	}
	if usageRepo.lastLog.CommissionRate == nil {
		t.Fatal("commission_rate not snapshotted on openai path")
	}
	if math.Abs(*usageRepo.lastLog.CommissionRate-0.08) > 1e-9 {
		t.Fatalf("want 0.08, got %v", *usageRepo.lastLog.CommissionRate)
	}
}
