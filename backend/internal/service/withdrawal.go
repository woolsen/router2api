package service

import (
	"context"
	"math"
	"strings"
	"time"

	infraerrors "github.com/woolsen/router2api/internal/pkg/errors"
)

const (
	// WithdrawalStatusRequested 商户已提交，提现金额已从可用余额转入冻结。
	WithdrawalStatusRequested = "requested"
	// WithdrawalStatusApproved 运营已审核通过，等待打款；资金仍在冻结中。
	WithdrawalStatusApproved = "approved"
	// WithdrawalStatusPaid 已打款，终态；冻结额在打款时释放。
	WithdrawalStatusPaid = "paid"
	// WithdrawalStatusRejected 运营驳回，冻结额退回可用余额。
	WithdrawalStatusRejected = "rejected"

	// WithdrawalMinimumAmount 是单笔提现的最低金额（美元）。
	WithdrawalMinimumAmount = 50.0

	withdrawalAmountScale = 1e10 // money 列为 decimal(20,10)
)

var (
	// ErrBelowMinimum 表示提现金额低于最低提现额。
	ErrBelowMinimum = infraerrors.BadRequest("WITHDRAWAL_BELOW_MINIMUM", "withdrawal amount is below the $50 minimum")
	// ErrAmountExceedsAvailable 表示提现金额超过 merchant_balance.balance。
	ErrAmountExceedsAvailable = infraerrors.BadRequest("WITHDRAWAL_EXCEEDS_AVAILABLE", "withdrawal amount exceeds available balance")
	// ErrWithdrawalAmountInvalid 表示金额不是有限正数。
	ErrWithdrawalAmountInvalid = infraerrors.BadRequest("WITHDRAWAL_AMOUNT_INVALID", "withdrawal amount must be a finite positive number")
	// ErrWithdrawalInFlight 表示同一商户已有一笔在途提现。
	// 可用额可能仍足够，但部分唯一索引只允许一笔 requested/approved。
	ErrWithdrawalInFlight = infraerrors.Conflict("WITHDRAWAL_IN_FLIGHT", "another withdrawal is already in progress for this merchant")
	// ErrWithdrawalNotFound 提现单不存在。
	ErrWithdrawalNotFound = infraerrors.NotFound("WITHDRAWAL_NOT_FOUND", "withdrawal not found")
	// ErrAlreadyPaid 表示提现单不在 approved 状态（已打款或尚未审核）。
	// MarkPaid 用条件更新 WHERE status='approved'，0 行一律按此错误返回。
	ErrAlreadyPaid = infraerrors.Conflict("WITHDRAWAL_ALREADY_PAID", "withdrawal is already paid or not approved for payout")
	// ErrWithdrawalStatusInvalid 表示运营端传入了未知的提现状态过滤器。
	ErrWithdrawalStatusInvalid = infraerrors.BadRequest("WITHDRAWAL_STATUS_INVALID", "unknown withdrawal status")
	// ErrWithdrawalStatusTransition 表示当前状态不允许该操作。
	ErrWithdrawalStatusTransition = infraerrors.Conflict("WITHDRAWAL_STATUS_TRANSITION", "withdrawal status does not allow this operation")
)

// Withdrawal 是商户的一笔提现申请。
// 资金流转：requested 时 balance -= amount、frozen += amount；
// rejected 时反向解冻；paid 时只释放 frozen，balance 不再变化。
type Withdrawal struct {
	ID          int64
	MerchantID  int64
	Amount      float64
	Status      string
	ProofPath   string
	ProofSHA256 string
	ApprovedBy  string
	RequestedAt time.Time
	PaidAt      *time.Time
}

// WithdrawalRepository 是商户钱包与提现的持久化端口。
// 所有余额变更都必须由实现用事务 + 行锁完成，服务层不做资金计算。
type WithdrawalRepository interface {
	// Available 直接读 merchant_balance.balance；无账本行返回 0。
	Available(ctx context.Context, merchantID int64) (float64, error)
	// List 返回商户自己的提现单，按申请时间倒序。
	List(ctx context.Context, merchantID int64) ([]Withdrawal, error)
	// ListAll 返回运营端提现单；status 为空时返回全部。
	ListAll(ctx context.Context, status string) ([]Withdrawal, error)
	// Request 锁定余额行后校验可用额与在途提现，并原子写入提现单、冻结余额、记账。
	Request(ctx context.Context, merchantID int64, amount float64, at time.Time) (*Withdrawal, error)
	// Approve 把 requested 推进到 approved；已 approved 幂等。
	Approve(ctx context.Context, withdrawalID int64, by string, at time.Time) error
	// Reject 把 requested/approved 推进到 rejected，并解冻金额；已 rejected 幂等。
	Reject(ctx context.Context, withdrawalID int64, by string, at time.Time) error
	// MarkPaid 用条件更新把 approved 推进到 paid；0 行返回 ErrAlreadyPaid。
	MarkPaid(ctx context.Context, withdrawalID int64, proofPath, sha256, by string, at time.Time) error
}

// WithdrawalService 负责提现的领域校验与用例编排。
type WithdrawalService struct {
	repo WithdrawalRepository
	now  func() time.Time
}

func NewWithdrawalService(repo WithdrawalRepository) *WithdrawalService {
	return &WithdrawalService{repo: repo, now: time.Now}
}

// Available 返回可提现余额，唯一来源是 merchant_balance.balance。
// 不使用“已确认结算减提现”的推导口径：两套账本必然漂移。
func (s *WithdrawalService) Available(ctx context.Context, merchantID int64) (float64, error) {
	if s == nil || s.repo == nil || merchantID <= 0 {
		return 0, ErrWithdrawalNotFound
	}
	return s.repo.Available(ctx, merchantID)
}

// List 返回商户自己的提现记录。
func (s *WithdrawalService) List(ctx context.Context, merchantID int64) ([]Withdrawal, error) {
	if s == nil || s.repo == nil || merchantID <= 0 {
		return nil, ErrWithdrawalNotFound
	}
	return s.repo.List(ctx, merchantID)
}

// ListAll 返回运营端提现记录；status 必须是已知状态或空。
func (s *WithdrawalService) ListAll(ctx context.Context, status string) ([]Withdrawal, error) {
	if s == nil || s.repo == nil {
		return nil, ErrWithdrawalNotFound
	}
	status = strings.TrimSpace(status)
	switch status {
	case "", WithdrawalStatusRequested, WithdrawalStatusApproved, WithdrawalStatusPaid, WithdrawalStatusRejected:
	default:
		return nil, ErrWithdrawalStatusInvalid
	}
	return s.repo.ListAll(ctx, status)
}

// Request 创建提现申请。金额校验在服务层；余额与在途校验在仓储事务内完成，
// 以免并发请求基于过期余额各自扣款。
func (s *WithdrawalService) Request(ctx context.Context, merchantID int64, amount float64) (*Withdrawal, error) {
	if s == nil || s.repo == nil || merchantID <= 0 {
		return nil, ErrWithdrawalNotFound
	}
	if math.IsNaN(amount) || math.IsInf(amount, 0) || amount <= 0 {
		return nil, ErrWithdrawalAmountInvalid
	}
	amount = math.Round(amount*withdrawalAmountScale) / withdrawalAmountScale
	if amount < WithdrawalMinimumAmount {
		return nil, ErrBelowMinimum
	}
	return s.repo.Request(ctx, merchantID, amount, s.now().UTC())
}

// Approve 审核通过一笔提现，资金保持冻结直到打款。
func (s *WithdrawalService) Approve(ctx context.Context, withdrawalID int64, by string) error {
	if s == nil || s.repo == nil || withdrawalID <= 0 {
		return ErrWithdrawalNotFound
	}
	return s.repo.Approve(ctx, withdrawalID, withdrawalActor(by), s.now().UTC())
}

// Reject 驳回提现并解冻：balance += amount、frozen -= amount。
func (s *WithdrawalService) Reject(ctx context.Context, withdrawalID int64, by string) error {
	if s == nil || s.repo == nil || withdrawalID <= 0 {
		return ErrWithdrawalNotFound
	}
	return s.repo.Reject(ctx, withdrawalID, withdrawalActor(by), s.now().UTC())
}

// MarkPaid 记录打款凭证并完成提现。
// 仓储必须使用 WHERE status='approved' 的条件更新；0 行意味着已打款或尚未审核，
// 返回 ErrAlreadyPaid 而不是静默成功，避免运营把失败当成已完成。
func (s *WithdrawalService) MarkPaid(ctx context.Context, withdrawalID int64, proofPath, sha256, by string) error {
	if s == nil || s.repo == nil || withdrawalID <= 0 {
		return ErrWithdrawalNotFound
	}
	return s.repo.MarkPaid(ctx, withdrawalID, strings.TrimSpace(proofPath), strings.TrimSpace(sha256), withdrawalActor(by), s.now().UTC())
}

func withdrawalActor(by string) string {
	actor := strings.TrimSpace(by)
	if actor == "" {
		return SettlementDefaultOperator
	}
	return actor
}
