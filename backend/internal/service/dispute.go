package service

import (
	"context"
	"math"
	"strings"
	"time"

	infraerrors "github.com/woolsen/router2api/internal/pkg/errors"
)

const (
	// DisputeOwnerMerchant 表示申诉由商户发起，目标只能是自己的结算单。
	DisputeOwnerMerchant = "merchant"
	// DisputeOwnerUser 表示申诉由普通用户发起，目标只能是自己的单条用量。
	DisputeOwnerUser = "user"

	// DisputeTargetSettlement 表示目标是一张结算单。
	DisputeTargetSettlement = "settlement"
	// DisputeTargetUsage 表示目标是单条 usage_logs 记录。
	DisputeTargetUsage = "usage"

	// DisputeStatusOpen 表示等待运营裁决；同一目标同时只能有一条 open 申诉。
	DisputeStatusOpen = "open"
	// DisputeStatusResolved 表示运营已裁决。
	DisputeStatusResolved = "resolved"

	// DisputeDecisionReject 驳回申诉：冻结额原路释放，结算单回到裁决前状态。
	DisputeDecisionReject = "reject"
	// DisputeDecisionConfirm 认可申诉但不改金额：结算单确认，冻结额原路释放。
	DisputeDecisionConfirm = "confirm"
	// DisputeDecisionAdjust 调账：按差异金额扣减/补足，并把 adjust 流水挂到申诉单。
	DisputeDecisionAdjust = "adjust"

	// DisputeDefaultOperator 是未提供操作者时的兜底标识。
	DisputeDefaultOperator = "operator"

	disputeAmountScale = 1e10 // merchant_balance / merchant_ledger 为 decimal(20,10)
)

var (
	// ErrDisputeNotFound 申诉单不存在。
	ErrDisputeNotFound = infraerrors.NotFound("DISPUTE_NOT_FOUND", "dispute not found")
	// ErrDisputeTargetNotFound 目标不存在或不属于当前发起方。
	// 越权访问统一返回 404，避免通过 403 泄露目标是否存在。
	ErrDisputeTargetNotFound = infraerrors.NotFound("DISPUTE_TARGET_NOT_FOUND", "dispute target not found")
	// ErrDisputeWindowClosed 已过结算单申诉窗口。
	ErrDisputeWindowClosed = infraerrors.Conflict("DISPUTE_WINDOW_CLOSED", "settlement dispute window is closed")
	// ErrDisputeAlreadyOpen 同一目标已有 open 申诉。
	ErrDisputeAlreadyOpen = infraerrors.Conflict("DISPUTE_ALREADY_OPEN", "an open dispute already exists for this target")
	// ErrDisputeAlreadyResolved 申诉已被裁决，不能重复裁决。
	ErrDisputeAlreadyResolved = infraerrors.Conflict("DISPUTE_ALREADY_RESOLVED", "dispute is already resolved")
	// ErrDisputeResolveConflict 条件更新未命中，说明并发修改已发生。
	ErrDisputeResolveConflict = infraerrors.Conflict("DISPUTE_RESOLVE_CONFLICT", "dispute or target changed concurrently")
	// ErrDisputeSettlementPaid 已打款结算单不允许申诉。
	ErrDisputeSettlementPaid = infraerrors.Conflict("DISPUTE_SETTLEMENT_PAID", "paid settlement cannot be disputed")
	// ErrDisputeTargetTypeInvalid 目标类型与发起方类型不匹配。
	ErrDisputeTargetTypeInvalid = infraerrors.BadRequest("DISPUTE_TARGET_TYPE_INVALID", "invalid dispute target type")
	// ErrDisputeReasonRequired 申诉原因不能为空。
	ErrDisputeReasonRequired = infraerrors.BadRequest("DISPUTE_REASON_REQUIRED", "dispute reason is required")
	// ErrDisputeAmountInvalid 差异金额不是有限非负数。
	ErrDisputeAmountInvalid = infraerrors.BadRequest("DISPUTE_AMOUNT_INVALID", "dispute amount must be a finite non-negative number")
	// ErrDisputeDecisionInvalid 裁决结果未知。
	ErrDisputeDecisionInvalid = infraerrors.BadRequest("DISPUTE_DECISION_INVALID", "dispute decision must be reject, confirm or adjust")
	// ErrDisputeStatusInvalid 列表过滤条件未知。
	ErrDisputeStatusInvalid = infraerrors.BadRequest("DISPUTE_STATUS_INVALID", "unknown dispute status")
	// ErrAdjustRequiresSource 调账必须关联 dispute，禁止无来源调账。
	ErrAdjustRequiresSource = infraerrors.BadRequest("ADJUST_REQUIRES_SOURCE", "merchant adjustment must reference a dispute")
)

// Dispute 是一条申诉记录。
type Dispute struct {
	ID           int64
	OwnerType    string
	OwnerID      int64
	TargetType   string
	TargetID     int64
	Amount       float64
	Reason       string
	Status       string
	Decision     string
	AdjustAmount float64
	Note         string
	ResolvedBy   string
	CreatedAt    time.Time
	ResolvedAt   *time.Time
}

// OpenDisputeInput 是发起申诉的输入。
type OpenDisputeInput struct {
	OwnerType  string
	OwnerID    int64
	TargetType string
	TargetID   int64
	Amount     float64
	Reason     string
}

// ResolveInput 是运营裁决的输入。
// AdjustAmount 是对商户余额/用户余额的带符号增量：负数为扣减，正数为补足。
type ResolveInput struct {
	Decision     string
	AdjustAmount float64
	Note         string
	By           string
}

// DisputeRepository 是申诉的持久化端口。
// 所有状态变更必须用条件更新（WHERE status=...），0 行即并发冲突；
// 资金动作必须与状态变更在同一事务内完成。
type DisputeRepository interface {
	OpenSettlement(ctx context.Context, in OpenDisputeInput, at time.Time) (*Dispute, error)
	OpenUsage(ctx context.Context, in OpenDisputeInput, at time.Time) (*Dispute, error)
	ListByMerchant(ctx context.Context, merchantID int64) ([]Dispute, error)
	ListByUser(ctx context.Context, userID int64) ([]Dispute, error)
	ListAll(ctx context.Context, status string) ([]Dispute, error)
	Resolve(ctx context.Context, id int64, in ResolveInput, at time.Time) (*Dispute, error)
	// AdjustLedger 写一条必须关联 dispute 的商户调账流水。
	AdjustLedger(ctx context.Context, merchantID int64, amount float64, refType string, refID int64, at time.Time) error
}

// DisputeService 承担申诉的领域校验与用例编排。
type DisputeService struct {
	repo DisputeRepository
	now  func() time.Time
}

func NewDisputeService(repo DisputeRepository) *DisputeService {
	return &DisputeService{repo: repo, now: time.Now}
}

// Open 发起申诉。商户只能申诉自己的结算单，用户只能申诉自己的单条用量。
// 目标归属与窗口判断在仓储事务内完成，避免先查后改的竞态。
func (s *DisputeService) Open(ctx context.Context, in OpenDisputeInput) (*Dispute, error) {
	if s == nil || s.repo == nil {
		return nil, ErrDisputeTargetNotFound
	}

	in.Reason = strings.TrimSpace(in.Reason)
	if in.Reason == "" {
		return nil, ErrDisputeReasonRequired
	}
	if math.IsNaN(in.Amount) || math.IsInf(in.Amount, 0) || in.Amount < 0 {
		return nil, ErrDisputeAmountInvalid
	}
	in.Amount = roundDisputeAmount(in.Amount)

	switch in.TargetType {
	case DisputeTargetSettlement:
		if in.OwnerType != DisputeOwnerMerchant || in.OwnerID <= 0 || in.TargetID <= 0 {
			return nil, ErrDisputeTargetTypeInvalid
		}
		return s.repo.OpenSettlement(ctx, in, s.now().UTC())
	case DisputeTargetUsage:
		if in.OwnerType != DisputeOwnerUser || in.OwnerID <= 0 || in.TargetID <= 0 {
			return nil, ErrDisputeTargetTypeInvalid
		}
		return s.repo.OpenUsage(ctx, in, s.now().UTC())
	default:
		return nil, ErrDisputeTargetTypeInvalid
	}
}

// ListByMerchant 返回商户自己的申诉。
func (s *DisputeService) ListByMerchant(ctx context.Context, merchantID int64) ([]Dispute, error) {
	if s == nil || s.repo == nil || merchantID <= 0 {
		return nil, ErrDisputeTargetNotFound
	}
	return s.repo.ListByMerchant(ctx, merchantID)
}

// ListByUser 返回用户自己的申诉。
func (s *DisputeService) ListByUser(ctx context.Context, userID int64) ([]Dispute, error) {
	if s == nil || s.repo == nil || userID <= 0 {
		return nil, ErrDisputeTargetNotFound
	}
	return s.repo.ListByUser(ctx, userID)
}

// ListAll 返回运营端申诉列表；status 为空时返回全部。
func (s *DisputeService) ListAll(ctx context.Context, status string) ([]Dispute, error) {
	if s == nil || s.repo == nil {
		return nil, ErrDisputeNotFound
	}
	status = strings.ToLower(strings.TrimSpace(status))
	switch status {
	case "", DisputeStatusOpen, DisputeStatusResolved:
	default:
		return nil, ErrDisputeStatusInvalid
	}
	return s.repo.ListAll(ctx, status)
}

// Resolve 裁决一条 open 申诉。
//
// 金额语义由仓储按来源区分：
//   - 来源 A（pending → disputed，从未入账）：确认时补写唯一 settle 流水并入账；
//   - 来源 B（confirmed → disputed，已入账并冻结）：确认/驳回只释放冻结，
//     绝不重复写 settle。
func (s *DisputeService) Resolve(ctx context.Context, id int64, in ResolveInput) (*Dispute, error) {
	if s == nil || s.repo == nil || id <= 0 {
		return nil, ErrDisputeNotFound
	}

	in.Decision = strings.ToLower(strings.TrimSpace(in.Decision))
	switch in.Decision {
	case DisputeDecisionReject, DisputeDecisionConfirm, DisputeDecisionAdjust:
	default:
		return nil, ErrDisputeDecisionInvalid
	}
	if math.IsNaN(in.AdjustAmount) || math.IsInf(in.AdjustAmount, 0) {
		return nil, ErrDisputeAmountInvalid
	}
	in.AdjustAmount = roundDisputeAmount(in.AdjustAmount)
	if in.Decision != DisputeDecisionAdjust {
		in.AdjustAmount = 0
	}
	in.Note = strings.TrimSpace(in.Note)
	in.By = strings.TrimSpace(in.By)
	if in.By == "" {
		in.By = DisputeDefaultOperator
	}

	return s.repo.Resolve(ctx, id, in, s.now().UTC())
}

// AdjustLedger 是唯一允许写 merchant_ledger(type='adjust') 的入口。
// 无 dispute 来源的调账直接拒绝，避免出现无法追溯的资金变动。
func (s *DisputeService) AdjustLedger(
	ctx context.Context,
	merchantID int64,
	amount float64,
	refType string,
	refID int64,
) error {
	if s == nil || s.repo == nil || merchantID <= 0 {
		return ErrDisputeTargetNotFound
	}
	if strings.TrimSpace(refType) != "dispute" || refID <= 0 {
		return ErrAdjustRequiresSource
	}
	if math.IsNaN(amount) || math.IsInf(amount, 0) {
		return ErrDisputeAmountInvalid
	}
	return s.repo.AdjustLedger(ctx, merchantID, roundDisputeAmount(amount), "dispute", refID, s.now().UTC())
}

func roundDisputeAmount(v float64) float64 {
	return math.Round(v*disputeAmountScale) / disputeAmountScale
}
