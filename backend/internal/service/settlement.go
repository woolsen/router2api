package service

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/woolsen/router2api/internal/config"
)

const (
	// SettlementStatusPending 已生成，等待商户确认。
	SettlementStatusPending = "pending"
	// SettlementStatusConfirmed 商户已确认，等待打款。
	SettlementStatusConfirmed = "confirmed"
	// SettlementStatusPaid 已打款，终态。
	SettlementStatusPaid = "paid"

	// SettlementCloseDelay 是账期结束后的封账冷却期。
	// usage_logs 异步 best-effort 落库，只有 now >= period_end + 24h 才允许生成该期；
	// 聚合本身仍覆盖完整账期 [period_start, period_end)，不从右侧截断，
	// 否则每期最后 24h 会永久落不进任何一期。
	SettlementCloseDelay = 24 * time.Hour

	settlementWeek = 7 * 24 * time.Hour
)

var (
	// ErrSettlementMerchantRequired 表示没有指定要结算的商户。
	ErrSettlementMerchantRequired = errors.New("settlement merchant id is required")
	// ErrSettlementPeriodInvalid 表示账期起点不早于终点。
	ErrSettlementPeriodInvalid = errors.New("settlement period start must be before period end")
	// ErrSettlementPeriodNotClosed 表示账期尚未结束满 24h，不能封账。
	ErrSettlementPeriodNotClosed = errors.New("settlement period is still open: period_end + 24h not reached")
	// ErrSettlementSimpleMode 表示 simple 模式下用量未真实扣费，禁止生成结算单。
	ErrSettlementSimpleMode = errors.New("settlement generation is disabled when run_mode=simple")
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
	// EarliestSettleableUsageAt 返回该商户最早一条可结算用量的时间；无用量返回 nil。
	EarliestSettleableUsageAt(ctx context.Context, merchantID int64) (*time.Time, error)
	// ListSettledPeriodStarts 返回该商户已生成结算单的账期起点（UTC）。
	ListSettledPeriodStarts(ctx context.Context, merchantID int64) ([]time.Time, error)
}

// SettlementService 负责按账期聚合用量并生成结算单。
type SettlementService struct {
	repo     SettlementRepository
	cfg      *config.Config
	interval time.Duration
	now      func() time.Time

	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

func NewSettlementService(repo SettlementRepository, cfg *config.Config) *SettlementService {
	return &SettlementService{
		repo:     repo,
		cfg:      cfg,
		interval: time.Hour,
		now:      time.Now,
		stopCh:   make(chan struct{}),
	}
}

// Generate 生成（或返回已存在的）商户周结算单。
//
// 账期是 [periodStart, periodEnd) 的完整区间；usage_logs 异步落库的缓冲放在时间轴上：
// 只有 now >= periodEnd + 24h 才允许封账。periodStart/periodEnd 一律按 UTC 存储。
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
	if s.simpleMode() {
		return nil, ErrSettlementSimpleMode
	}
	if s.now().Before(end.Add(SettlementCloseDelay)) {
		return nil, ErrSettlementPeriodNotClosed
	}

	agg, err := s.repo.AggregateUsage(ctx, merchantID, start, end)
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

func (s *SettlementService) simpleMode() bool {
	return s != nil && s.cfg != nil && s.cfg.RunMode == config.RunModeSimple
}

// Start 启动周结算 ticker：每小时检查一次，每个账期只生成一次。
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

// runOnce 为每个活跃商户补齐从最早未结算周到最后一个可封账周的结算单。
// 单个商户失败只影响该商户，下一 tick 会从它最早的未结算周继续重试。
func (s *SettlementService) runOnce() {
	if s.simpleMode() {
		log.Printf("[Settlement] ERROR: refusing to generate settlements: run_mode=simple does not charge usage; market mode requires run_mode=standard")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	lastSettleableEnd := latestSettleablePeriodEnd(s.now())
	ids, err := s.repo.ListActiveMerchantIDs(ctx)
	if err != nil {
		log.Printf("[Settlement] list active merchants failed: %v", err)
		return
	}

	for _, id := range ids {
		if err := s.settleMerchant(ctx, id, lastSettleableEnd); err != nil {
			log.Printf("[Settlement] merchant %d catch-up failed (up to %s): %v",
				id, lastSettleableEnd.Format(time.RFC3339), err)
		}
	}
}

// settleMerchant 从该商户最早的可结算用量推导起始周，逐周补齐到 lastSettleableEnd。
// 已存在的账期直接跳过，因此重跑不会重复生成，也不会漏掉中间失败的周。
func (s *SettlementService) settleMerchant(ctx context.Context, merchantID int64, lastSettleableEnd time.Time) error {
	earliest, err := s.repo.EarliestSettleableUsageAt(ctx, merchantID)
	if err != nil {
		return err
	}
	if earliest == nil {
		return nil
	}

	settled, err := s.repo.ListSettledPeriodStarts(ctx, merchantID)
	if err != nil {
		return err
	}
	settledStarts := make(map[int64]struct{}, len(settled))
	for _, start := range settled {
		settledStarts[start.UTC().Unix()] = struct{}{}
	}

	start := startOfUTCWeek(*earliest)
	for ; ; start = start.Add(settlementWeek) {
		end := start.Add(settlementWeek)
		if end.After(lastSettleableEnd) {
			break
		}
		if _, ok := settledStarts[start.Unix()]; ok {
			continue
		}
		if _, err := s.Generate(ctx, merchantID, start, end); err != nil {
			return err
		}
	}
	return nil
}

// latestSettleablePeriodEnd 返回当前允许封账的最后一个周一 00:00（UTC）边界。
//
// 自然周 [M-7d, M) 的 period_end 是 M；只有 now >= M+24h 才允许生成，
// 因此在周一 00:00 之后的第一个 tick 才会生成刚结束的那一周。
func latestSettleablePeriodEnd(now time.Time) time.Time {
	weekStart := startOfUTCWeek(now)
	if now.Before(weekStart.Add(SettlementCloseDelay)) {
		return weekStart.Add(-settlementWeek)
	}
	return weekStart
}

// startOfUTCWeek 返回 t 所在周的周一 00:00:00 UTC。
func startOfUTCWeek(t time.Time) time.Time {
	t = t.UTC()
	day := (int(t.Weekday()) + 6) % 7 // Monday = 0
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -day)
}
