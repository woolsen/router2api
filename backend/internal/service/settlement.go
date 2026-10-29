package service

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"
)

const (
	// SettlementStatusPending 已生成，等待商户确认。
	SettlementStatusPending = "pending"
	// SettlementStatusConfirmed 商户已确认，等待打款。
	SettlementStatusConfirmed = "confirmed"
	// SettlementStatusPaid 已打款，终态。
	SettlementStatusPaid = "paid"

	// SettlementLateWriteBuffer 为 usage_logs 异步 best-effort 落库留出的窗口缓冲。
	// 只结 period_end - 24h 之前的行，避免把迟到写入算进已封账的账期；
	// 缓冲区内（最后一整天）的用量必须由补结流程补进下一期，否则会长期少付。
	SettlementLateWriteBuffer = 24 * time.Hour

	settlementWeek = 7 * 24 * time.Hour
)

var (
	// ErrSettlementMerchantRequired 表示没有指定要结算的商户。
	ErrSettlementMerchantRequired = errors.New("settlement merchant id is required")
	// ErrSettlementPeriodInvalid 表示账期起点不早于终点。
	ErrSettlementPeriodInvalid = errors.New("settlement period start must be before period end")
	// ErrSettlementStatusTransition 表示状态机不允许的流转。
	ErrSettlementStatusTransition = errors.New("invalid settlement status transition")
)

// Settlement 是商户的周结算应付单。
type Settlement struct {
	ID          int64
	MerchantID  int64
	PeriodStart time.Time
	PeriodEnd   time.Time
	GrossCost   float64
	Commission  float64
	NetPayable  float64
	Status      string
}

// CanAppeal 表示当前状态下是否还允许商户申诉。
func (s *Settlement) CanAppeal() bool {
	if s == nil {
		return false
	}
	return s.Status == SettlementStatusPending || s.Status == SettlementStatusConfirmed
}

// CanTransitionTo 校验状态机：pending → confirmed → paid，不允许跳级或回退。
func (s *Settlement) CanTransitionTo(next string) bool {
	if s == nil {
		return false
	}
	switch s.Status {
	case SettlementStatusPending:
		return next == SettlementStatusConfirmed
	case SettlementStatusConfirmed:
		return next == SettlementStatusPaid
	default:
		return false
	}
}

// TransitionTo 按状态机推进状态，非法流转返回 ErrSettlementStatusTransition。
func (s *Settlement) TransitionTo(next string) error {
	if !s.CanTransitionTo(next) {
		return ErrSettlementStatusTransition
	}
	s.Status = next
	return nil
}

// SettlementUsageAggregate 是账期内可结算用量的汇总结果。
// ExcludedUnknownCommissionRows 记录 commission_rate 为 NULL 而被排除的行数：
// 这些行写入时费率未知，不能按 0 结算，只能留待补结/人工核对。
type SettlementUsageAggregate struct {
	GrossCost                     float64
	Commission                    float64
	ExcludedUnknownCommissionRows int64
}

// SettlementRepository 是结算单的持久化接口。
type SettlementRepository interface {
	// AggregateUsage 汇总 [from, to) 内用户实付且费率已知的用量。
	AggregateUsage(ctx context.Context, merchantID int64, from, to time.Time) (SettlementUsageAggregate, error)
	// CreateOrGet 按 (merchant_id, period_start, period_end) 幂等创建；已存在则返回原行。
	CreateOrGet(ctx context.Context, s *Settlement) (*Settlement, error)
	// ListActiveMerchantIDs 返回需要参与周结算的商户。
	ListActiveMerchantIDs(ctx context.Context) ([]int64, error)
}

// SettlementService 负责按账期聚合用量并生成结算单。
type SettlementService struct {
	repo     SettlementRepository
	interval time.Duration
	now      func() time.Time

	mu            sync.Mutex
	lastPeriodEnd time.Time

	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

func NewSettlementService(repo SettlementRepository) *SettlementService {
	return &SettlementService{
		repo:     repo,
		interval: time.Hour,
		now:      time.Now,
		stopCh:   make(chan struct{}),
	}
}

// Generate 生成（或返回已存在的）商户周结算单。
//
// 聚合区间是 [periodStart, periodEnd-24h)：usage_logs 异步 best-effort 落库，
// 右侧留 24h 缓冲，避免把封账瞬间尚未落库的行漏在账期外；
// 缓冲区内（最后一整天）的用量需要后续补结进下一期，本期不结。
// periodStart/periodEnd 一律按 UTC 存储。
func (s *SettlementService) Generate(
	ctx context.Context,
	merchantID int64,
	periodStart, periodEnd time.Time,
) (*Settlement, error) {
	if merchantID <= 0 {
		return nil, ErrSettlementMerchantRequired
	}
	start, end := periodStart.UTC(), periodEnd.UTC()
	if !end.After(start) {
		return nil, ErrSettlementPeriodInvalid
	}

	agg, err := s.aggregate(ctx, merchantID, start, end)
	if err != nil {
		return nil, err
	}
	if agg.ExcludedUnknownCommissionRows > 0 {
		log.Printf(
			"[Settlement] merchant %d period %s..%s: %d usage rows skipped (commission_rate IS NULL); re-settle after rate backfill",
			merchantID, start.Format(time.RFC3339), end.Format(time.RFC3339), agg.ExcludedUnknownCommissionRows,
		)
	}

	return s.repo.CreateOrGet(ctx, &Settlement{
		MerchantID:  merchantID,
		PeriodStart: start,
		PeriodEnd:   end,
		GrossCost:   agg.GrossCost,
		Commission:  agg.Commission,
		NetPayable:  agg.GrossCost - agg.Commission,
		Status:      SettlementStatusPending,
	})
}

func (s *SettlementService) aggregate(
	ctx context.Context,
	merchantID int64,
	start, end time.Time,
) (SettlementUsageAggregate, error) {
	cutoff := end.Add(-SettlementLateWriteBuffer)
	if !cutoff.After(start) {
		// 账期短于缓冲，没有可结算的窗口。
		return SettlementUsageAggregate{}, nil
	}
	return s.repo.AggregateUsage(ctx, merchantID, start, cutoff)
}

// Start 启动周结算 ticker：每小时检查一次，每个账期最多生成一次。
func (s *SettlementService) Start() {
	if s == nil || s.repo == nil || s.interval <= 0 {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()
		s.runOnce()
		for {
			select {
			case <-ticker.C:
				s.runOnce()
			case <-s.stopCh:
				return
			}
		}
	}()
}

func (s *SettlementService) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() { close(s.stopCh) })
	s.wg.Wait()
}

// runOnce 为所有活跃商户生成上一个完整自然周（UTC 周一 00:00 边界）的结算单。
func (s *SettlementService) runOnce() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	periodEnd := startOfUTCWeek(s.now())
	periodStart := periodEnd.Add(-settlementWeek)

	s.mu.Lock()
	alreadyDone := !s.lastPeriodEnd.IsZero() && s.lastPeriodEnd.Equal(periodEnd)
	s.mu.Unlock()
	if alreadyDone {
		return
	}

	ids, err := s.repo.ListActiveMerchantIDs(ctx)
	if err != nil {
		log.Printf("[Settlement] list active merchants failed: %v", err)
		return
	}

	failed := 0
	for _, id := range ids {
		if _, err := s.Generate(ctx, id, periodStart, periodEnd); err != nil {
			failed++
			log.Printf("[Settlement] generate merchant %d period %s..%s failed: %v",
				id, periodStart.Format(time.RFC3339), periodEnd.Format(time.RFC3339), err)
		}
	}

	// 失败时保留重试机会（下一小时再跑）；成功才记住本账期。
	if failed == 0 {
		s.mu.Lock()
		s.lastPeriodEnd = periodEnd
		s.mu.Unlock()
	}
}

// startOfUTCWeek 返回 t 所在周的周一 00:00:00 UTC。
func startOfUTCWeek(t time.Time) time.Time {
	t = t.UTC()
	day := (int(t.Weekday()) + 6) % 7 // Monday = 0
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -day)
}
