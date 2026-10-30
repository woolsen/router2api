package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	// ErrSettlementNotFound 结算单不存在。
	ErrSettlementNotFound = errors.New("settlement not found")
	// ErrSettlementDisputed 结算单处于申诉中，裁决完成前不能确认。
	ErrSettlementDisputed = errors.New("settlement is disputed")
	// ErrSettlementPaid 结算单已打款，终态不允许再变更或重复入账。
	ErrSettlementPaid = errors.New("settlement is already paid")
	// ErrSettlementConflict 条件更新没有命中预期状态，说明并发修改已发生。
	ErrSettlementConflict = errors.New("settlement status changed concurrently")
	// ErrSettlementDisputeWindowClosed 已过 7 天申诉窗口，不能再发起申诉。
	ErrSettlementDisputeWindowClosed = errors.New("settlement dispute window is closed")
)

const (
	// SettlementAutoConfirmActor 是自动确认写入审计日志的操作者。
	SettlementAutoConfirmActor = "auto"
	// SettlementDefaultOperator 是手动操作未提供操作者时的兜底标识。
	SettlementDefaultOperator = "operator"
	// SettlementDisputeActor 是商户发起申诉时写入审计日志的操作者。
	SettlementDisputeActor = "merchant"

	settlementAutoConfirmBatchSize = 200
)

// SettlementItem 是结算单中的一条用量明细。
// cost 是用户实付，commission 是平台抽成；两者来自生成结算单时的 usage_logs 快照。
type SettlementItem struct {
	ID           int64
	SettlementID int64
	UsageLogID   int64
	Cost         float64
	Commission   float64
}

// SettlementTransition 描述一次条件状态流转的结果。
// Applied=false 时 Status 是读取到的当前状态，供 service 区分“已确认/已打款/申诉中”。
type SettlementTransition struct {
	Applied             bool
	Status              string
	DisputeWindowClosed bool
}

// SettlementOpsRepository 是结算单状态机与明细的持久化端口。
// 所有状态变更都必须由实现用条件更新（WHERE status=...）完成，0 行即并发冲突。
type SettlementOpsRepository interface {
	GetSettlement(ctx context.Context, settlementID int64) (*Settlement, error)
	ListSettlementItems(ctx context.Context, settlementID int64) ([]SettlementItem, error)
	// ListExpiredPendingSettlements 按 id 升序返回 status='pending'
	// 且 dispute_deadline < before、id > afterID 的结算单。
	ListExpiredPendingSettlements(ctx context.Context, before time.Time, afterID int64, limit int) ([]*Settlement, error)
	// ConfirmSettlement 原子完成 pending → confirmed 与商户余额入账。
	ConfirmSettlement(ctx context.Context, settlementID int64, by string, at time.Time) (SettlementTransition, error)
	// DisputeSettlement 原子完成 pending|confirmed → disputed。
	DisputeSettlement(ctx context.Context, settlementID int64, reason string, at time.Time) (SettlementTransition, error)
}

// SettlementOps 负责结算单明细查询、状态流转与 7 天自动确认。
type SettlementOps struct {
	repo SettlementOpsRepository
	now  func() time.Time
}

func NewSettlementOps(repo SettlementOpsRepository) *SettlementOps {
	return &SettlementOps{repo: repo, now: time.Now}
}

// Explore 返回结算单的用量明细；结算单不存在时返回 ErrSettlementNotFound。
func (o *SettlementOps) Explore(ctx context.Context, settlementID int64) ([]SettlementItem, error) {
	if o == nil || o.repo == nil || settlementID <= 0 {
		return nil, ErrSettlementNotFound
	}
	if _, err := o.repo.GetSettlement(ctx, settlementID); err != nil {
		return nil, err
	}
	return o.repo.ListSettlementItems(ctx, settlementID)
}

// Confirm 确认结算单并给商户余额入账。
// 重复确认（已 confirmed）是幂等的；disputed 必须等裁决完成，paid 是终态。
func (o *SettlementOps) Confirm(ctx context.Context, settlementID int64, by string) error {
	if o == nil || o.repo == nil || settlementID <= 0 {
		return ErrSettlementNotFound
	}
	actor := strings.TrimSpace(by)
	if actor == "" {
		actor = SettlementDefaultOperator
	}

	tr, err := o.repo.ConfirmSettlement(ctx, settlementID, actor, o.now().UTC())
	if err != nil {
		return err
	}
	if tr.Applied {
		return nil
	}
	switch tr.Status {
	case SettlementStatusConfirmed:
		return nil
	case SettlementStatusPaid:
		return ErrSettlementPaid
	case SettlementStatusDisputed:
		return ErrSettlementDisputed
	default:
		return fmt.Errorf("%w: %s", ErrSettlementStatusTransition, tr.Status)
	}
}

// Dispute 发起商户申诉：pending|confirmed → disputed，仅限 7 天窗口内且未打款。
// confirmed 的结算单已在确认时入账，申诉时由仓储在同一事务内把 net_payable
// 从可用余额转入冻结；pending 从未入账，不冻结。
func (o *SettlementOps) Dispute(ctx context.Context, settlementID int64, reason string) error {
	if o == nil || o.repo == nil || settlementID <= 0 {
		return ErrSettlementNotFound
	}
	current, err := o.repo.GetSettlement(ctx, settlementID)
	if err != nil {
		return err
	}
	switch current.Status {
	case SettlementStatusPending, SettlementStatusConfirmed:
		// 可申诉。
	case SettlementStatusDisputed:
		return ErrSettlementDisputed
	case SettlementStatusPaid:
		return ErrSettlementPaid
	default:
		return fmt.Errorf("%w: %s", ErrSettlementStatusTransition, current.Status)
	}

	at := o.now().UTC()
	if current.DisputeDeadline != nil && !at.Before(*current.DisputeDeadline) {
		return ErrSettlementDisputeWindowClosed
	}

	tr, err := o.repo.DisputeSettlement(ctx, settlementID, strings.TrimSpace(reason), at)
	if err != nil {
		return err
	}
	if tr.Applied {
		return nil
	}
	if tr.DisputeWindowClosed {
		return ErrSettlementDisputeWindowClosed
	}
	switch tr.Status {
	case SettlementStatusDisputed:
		return ErrSettlementDisputed
	case SettlementStatusPaid:
		return ErrSettlementPaid
	default:
		return fmt.Errorf("%w: %s", ErrSettlementStatusTransition, tr.Status)
	}
}

// AutoConfirmExpired 扫描 pending 且 dispute_deadline < now 的结算单并逐张确认。
// 返回本轮真正完成确认的结算单数量；单张并发冲突不阻塞其余结算单。
func (o *SettlementOps) AutoConfirmExpired(ctx context.Context) (int, error) {
	if o == nil || o.repo == nil {
		return 0, nil
	}
	now := o.now().UTC()
	total := 0
	var afterID int64
	for {
		pending, err := o.repo.ListExpiredPendingSettlements(ctx, now, afterID, settlementAutoConfirmBatchSize)
		if err != nil {
			return total, err
		}
		if len(pending) == 0 {
			return total, nil
		}
		for _, s := range pending {
			if s == nil {
				continue
			}
			afterID = s.ID
			if s.DisputeDeadline == nil || !s.DisputeDeadline.Before(now) {
				continue
			}
			tr, err := o.repo.ConfirmSettlement(ctx, s.ID, SettlementAutoConfirmActor, now)
			if err != nil {
				if errors.Is(err, ErrSettlementConflict) || errors.Is(err, ErrSettlementNotFound) {
					continue
				}
				return total, err
			}
			if tr.Applied {
				total++
			}
		}
		if len(pending) < settlementAutoConfirmBatchSize {
			return total, nil
		}
	}
}
