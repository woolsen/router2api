package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/woolsen/router2api/internal/service"
)

// disputeColumns 是 service.Dispute 的读取列，顺序必须与 disputeScanDest 一致。
// 可空列统一 COALESCE 成空串，避免 database/sql 把 NULL 扫进 string 失败。
const disputeColumns = `id, owner_type, owner_id, target_type, target_id, amount,
reason, status, COALESCE(decision, '') AS decision, adjust_amount,
COALESCE(note, '') AS note, COALESCE(resolved_by, '') AS resolved_by,
created_at, resolved_at`

const disputeLockQuery = `
SELECT ` + disputeColumns + `
FROM disputes
WHERE id = $1
FOR UPDATE`

const disputeListByMerchantQuery = `
SELECT ` + disputeColumns + `
FROM disputes
WHERE owner_type = 'merchant' AND owner_id = $1
ORDER BY created_at DESC, id DESC
LIMIT $2`

const disputeListByUserQuery = `
SELECT ` + disputeColumns + `
FROM disputes
WHERE owner_type = 'user' AND owner_id = $1
ORDER BY created_at DESC, id DESC
LIMIT $2`

const disputeListAllQuery = `
SELECT ` + disputeColumns + `
FROM disputes
WHERE ($1 = '' OR status = $1)
ORDER BY created_at DESC, id DESC
LIMIT $2`

const disputeInsertQuery = `
INSERT INTO disputes (
    owner_type, owner_id, target_type, target_id, amount, reason, status, created_at
) VALUES ($1, $2, $3, $4, $5, $6, 'open', $7)
RETURNING ` + disputeColumns

// settlementForDisputeLockQuery 锁住结算单并读取裁决需要的资金快照。
const settlementForDisputeLockQuery = `
SELECT status, merchant_id, net_payable, dispute_deadline
FROM settlements
WHERE id = $1
FOR UPDATE`

const settlementHasSettleLedgerQuery = `
SELECT EXISTS(
    SELECT 1 FROM merchant_ledger
    WHERE type = 'settle' AND ref_type = 'settlement' AND ref_id = $1
)`

// settlementToDisputedUpdate 是条件更新：只有仍处于裁决前状态才允许冻结。
const settlementToDisputedUpdate = `
UPDATE settlements
SET status = 'disputed',
    updated_at = $2
WHERE id = $1
  AND status = $3`

// settlementResolveConfirmedUpdate 用于 disputed → confirmed。
// confirmed_at 只在首次确认时写入；来源 B 保留原确认时间。
const settlementResolveConfirmedUpdate = `
UPDATE settlements
SET status = 'confirmed',
    confirmed_at = COALESCE(confirmed_at, $2),
    updated_at = $2
WHERE id = $1
  AND status = 'disputed'`

// settlementResolvePendingUpdate 用于来源 A 的驳回：
// 回到 pending 并重新给 7 天窗口，绝不写 settle 流水。
const settlementResolvePendingUpdate = `
UPDATE settlements
SET status = 'pending',
    confirmed_at = NULL,
    dispute_deadline = $2,
    updated_at = $3
WHERE id = $1
  AND status = 'disputed'`

// disputeHoldLedgerInsert 以 dispute 为幂等键记录冻结。
// 不能沿用 Task 4 的 settlement 键：同一结算单可能经历多轮申诉，
// 每轮解冻后再次申诉必须能重新冻结。
const disputeHoldLedgerInsert = `
INSERT INTO merchant_ledger (merchant_id, type, amount, ref_type, ref_id, created_at)
SELECT merchant_id, 'dispute_hold', -net_payable, 'dispute', $2, $3
FROM settlements
WHERE id = $1
ON CONFLICT (type, ref_type, ref_id) DO NOTHING`

// disputeReleaseLedgerInsert 记录冻结释放：+net_payable 回到 balance。
// 以 dispute 为幂等键，每轮申诉至多释放一次。
const disputeReleaseLedgerInsert = `
INSERT INTO merchant_ledger (merchant_id, type, amount, ref_type, ref_id, created_at)
VALUES ($1, 'dispute_release', $2, 'dispute', $3, $4)
ON CONFLICT (type, ref_type, ref_id) DO NOTHING`

// disputeAdjustLedgerInsert 记录调账，必须引用 dispute。
const disputeAdjustLedgerInsert = `
INSERT INTO merchant_ledger (merchant_id, type, amount, ref_type, ref_id, created_at)
VALUES ($1, 'adjust', $2, 'dispute', $3, $4)
ON CONFLICT (type, ref_type, ref_id) DO NOTHING`

const disputeReleaseBalanceQuery = `
UPDATE merchant_balance
SET frozen = frozen - $2,
    balance = balance + $2,
    updated_at = $3
WHERE merchant_id = $1`

// disputeAdjustDeductFrozenQuery 先从未冻结额里扣除调账。
const disputeAdjustDeductFrozenQuery = `
UPDATE merchant_balance
SET frozen = frozen - $2,
    updated_at = $3
WHERE merchant_id = $1`

// disputeAdjustReleaseQuery 释放冻结余额的剩余部分。
// 调账为负时 remainder = net - |adjust|（可能为负，表示扣减超过本单净额，
// 由后续结算结转）；冻结额最终只减少 net。
const disputeAdjustReleaseQuery = `
UPDATE merchant_balance
SET frozen = frozen - $2,
    balance = balance + $3,
    updated_at = $4
WHERE merchant_id = $1`

// disputeAdjustCreditQuery 用于正调账，或来源 A（从未冻结）的调账。
const disputeAdjustCreditQuery = `
UPDATE merchant_balance
SET balance = balance + $2,
    updated_at = $3
WHERE merchant_id = $1`

const usageLogOwnerQuery = `
SELECT user_id
FROM usage_logs
WHERE id = $1`

// disputeUserBalanceQuery 给用户补余额；余额不允许为负。
const disputeUserBalanceQuery = `
UPDATE users
SET balance = balance + $1,
    updated_at = $2
WHERE id = $3
  AND deleted_at IS NULL
  AND balance + $1 >= 0`

const disputeUserExistsQuery = `
SELECT 1
FROM users
WHERE id = $1 AND deleted_at IS NULL`

const disputeOwnerQuery = `
SELECT owner_type, owner_id
FROM disputes
WHERE id = $1`

const disputeAuditInsert = `
INSERT INTO audit_logs (
    created_at, actor_email, actor_role, action, method, path, request_body, extra
) VALUES ($1, $2, 'system', $3, 'SYSTEM', $4, '', $5::jsonb)`

const disputeResolveUpdate = `
UPDATE disputes
SET status = 'resolved',
    decision = $2,
    adjust_amount = $3,
    note = $4,
    resolved_by = $5,
    resolved_at = $6
WHERE id = $1
  AND status = 'open'`

type disputeRepository struct {
	sql sqlExecutor
	db  sqlBeginner
}

func NewDisputeRepository(sqlDB *sql.DB) service.DisputeRepository {
	return newDisputeRepositoryWithSQL(sqlDB)
}

func newDisputeRepositoryWithSQL(sqlq sqlExecutor) *disputeRepository {
	repo := &disputeRepository{sql: sqlq}
	if beginner, ok := sqlq.(sqlBeginner); ok {
		repo.db = beginner
	}
	return repo
}

func (r *disputeRepository) beginTx(ctx context.Context) (*sql.Tx, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("dispute repository is not transactional")
	}
	return r.db.BeginTx(ctx, nil)
}

func disputeScanDest(d *service.Dispute) []any {
	return []any{
		&d.ID,
		&d.OwnerType,
		&d.OwnerID,
		&d.TargetType,
		&d.TargetID,
		&d.Amount,
		&d.Reason,
		&d.Status,
		&d.Decision,
		&d.AdjustAmount,
		&d.Note,
		&d.ResolvedBy,
		&d.CreatedAt,
		&d.ResolvedAt,
	}
}

func scanDisputeRow(row interface{ Scan(dest ...any) error }) (*service.Dispute, error) {
	var d service.Dispute
	if err := row.Scan(disputeScanDest(&d)...); err != nil {
		return nil, err
	}
	return &d, nil
}

func (r *disputeRepository) listDisputes(
	ctx context.Context,
	query string,
	args ...any,
) ([]service.Dispute, error) {
	rows, err := r.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	items := make([]service.Dispute, 0)
	for rows.Next() {
		d, err := scanDisputeRow(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, *d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

func (r *disputeRepository) ListByMerchant(ctx context.Context, merchantID int64) ([]service.Dispute, error) {
	return r.listDisputes(ctx, disputeListByMerchantQuery, merchantID, 200)
}

func (r *disputeRepository) ListByUser(ctx context.Context, userID int64) ([]service.Dispute, error) {
	return r.listDisputes(ctx, disputeListByUserQuery, userID, 200)
}

func (r *disputeRepository) ListAll(ctx context.Context, status string) ([]service.Dispute, error) {
	return r.listDisputes(ctx, disputeListAllQuery, status, 200)
}

// OpenSettlement 在结算单行锁内完成：归属/窗口/状态校验 → 写 dispute →
// pending|confirmed → disputed。confirmed 来源已在确认时入账，
// 必须在同一事务内把净额从可用余额转入冻结；pending 来源从未入账，不冻结。
func (r *disputeRepository) OpenSettlement(
	ctx context.Context,
	in service.OpenDisputeInput,
	at time.Time,
) (*service.Dispute, error) {
	tx, err := r.beginTx(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var (
		status     string
		merchantID int64
		netPayable float64
		deadline   *time.Time
	)
	err = tx.QueryRowContext(ctx, settlementForDisputeLockQuery, in.TargetID).
		Scan(&status, &merchantID, &netPayable, &deadline)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, service.ErrDisputeTargetNotFound
		}
		return nil, err
	}
	if merchantID != in.OwnerID {
		return nil, service.ErrDisputeTargetNotFound
	}
	switch status {
	case service.SettlementStatusPending, service.SettlementStatusConfirmed:
	case service.SettlementStatusDisputed:
		return nil, service.ErrDisputeAlreadyOpen
	case service.SettlementStatusPaid:
		return nil, service.ErrDisputeSettlementPaid
	default:
		return nil, service.ErrSettlementConflict
	}
	if deadline == nil || !at.UTC().Before(deadline.UTC()) {
		return nil, service.ErrDisputeWindowClosed
	}

	d, err := scanDisputeRow(tx.QueryRowContext(ctx, disputeInsertQuery,
		in.OwnerType, in.OwnerID, service.DisputeTargetSettlement, in.TargetID,
		in.Amount, in.Reason, at.UTC()))
	if err != nil {
		if isUniqueConstraintViolation(err) {
			return nil, service.ErrDisputeAlreadyOpen
		}
		return nil, err
	}

	res, err := tx.ExecContext(ctx, settlementToDisputedUpdate, in.TargetID, at.UTC(), status)
	if err != nil {
		return nil, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if affected == 0 {
		return nil, service.ErrSettlementConflict
	}

	if status == service.SettlementStatusConfirmed {
		if err := holdSettlementForDispute(ctx, tx, in.TargetID, d.ID, merchantID, netPayable, at); err != nil {
			return nil, err
		}
	}

	if err := insertDisputeAudit(ctx, tx, "dispute.opened", d, in.OwnerType, at); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return d, nil
}

// holdSettlementForDispute 先写 dispute 维度的冻结流水，只有流水真正插入时才调整余额。
func holdSettlementForDispute(
	ctx context.Context,
	tx *sql.Tx,
	settlementID, disputeID, merchantID int64,
	netPayable float64,
	at time.Time,
) error {
	res, err := tx.ExecContext(ctx, disputeHoldLedgerInsert, settlementID, disputeID, at.UTC())
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return nil
	}

	var balanceAfter float64
	if err := tx.QueryRowContext(ctx, settlementDisputeHoldBalance,
		settlementID, at.UTC()).Scan(&balanceAfter); err != nil {
		return err
	}
	if balanceAfter < 0 {
		log.Printf(
			"[Dispute] hold overdrew merchant %d: settlement %d dispute %d net %.10f, balance after hold %.10f < 0; the merchant may already have withdrawn these funds, manual reconciliation required",
			merchantID, settlementID, disputeID, netPayable, balanceAfter)
	}
	return nil
}

// OpenUsage 校验 usage_logs 归属后写 dispute。
// 用户只能申诉自己的单笔扣费；越权统一返回 404。
func (r *disputeRepository) OpenUsage(
	ctx context.Context,
	in service.OpenDisputeInput,
	at time.Time,
) (*service.Dispute, error) {
	tx, err := r.beginTx(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var ownerID int64
	err = tx.QueryRowContext(ctx, usageLogOwnerQuery, in.TargetID).Scan(&ownerID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, service.ErrDisputeTargetNotFound
		}
		return nil, err
	}
	if ownerID != in.OwnerID {
		return nil, service.ErrDisputeTargetNotFound
	}

	d, err := scanDisputeRow(tx.QueryRowContext(ctx, disputeInsertQuery,
		in.OwnerType, in.OwnerID, service.DisputeTargetUsage, in.TargetID,
		in.Amount, in.Reason, at.UTC()))
	if err != nil {
		if isUniqueConstraintViolation(err) {
			return nil, service.ErrDisputeAlreadyOpen
		}
		return nil, err
	}

	if err := insertDisputeAudit(ctx, tx, "dispute.opened", d, in.OwnerType, at); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return d, nil
}

// Resolve 在同一个事务内完成状态流转、资金处理与审计。
// 先锁 dispute，再锁目标；dispute 的最终条件更新放在最后，
// 任何一步失败都会整体回滚。
func (r *disputeRepository) Resolve(
	ctx context.Context,
	id int64,
	in service.ResolveInput,
	at time.Time,
) (*service.Dispute, error) {
	tx, err := r.beginTx(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	d, err := scanDisputeRow(tx.QueryRowContext(ctx, disputeLockQuery, id))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, service.ErrDisputeNotFound
		}
		return nil, err
	}
	if d.Status != service.DisputeStatusOpen {
		return nil, service.ErrDisputeAlreadyResolved
	}
	// 资金分支通过 d 读取本次裁决结果；必须先写入，否则会用到表里的旧值。
	d.Decision = in.Decision
	d.AdjustAmount = in.AdjustAmount
	d.Note = in.Note
	d.ResolvedBy = in.By

	switch d.TargetType {
	case service.DisputeTargetSettlement:
		if err := r.resolveSettlementDispute(ctx, tx, d, in, at); err != nil {
			return nil, err
		}
	case service.DisputeTargetUsage:
		if err := resolveUsageDispute(ctx, tx, d, in, at); err != nil {
			return nil, err
		}
	default:
		return nil, service.ErrDisputeTargetTypeInvalid
	}

	res, err := tx.ExecContext(ctx, disputeResolveUpdate,
		d.ID, in.Decision, in.AdjustAmount, in.Note, in.By, at.UTC())
	if err != nil {
		return nil, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if affected == 0 {
		return nil, service.ErrDisputeResolveConflict
	}

	d.Status = service.DisputeStatusResolved
	d.Decision = in.Decision
	d.AdjustAmount = in.AdjustAmount
	d.Note = in.Note
	d.ResolvedBy = in.By
	resolvedAt := at.UTC()
	d.ResolvedAt = &resolvedAt

	if err := insertDisputeAudit(ctx, tx, "dispute.resolved", d, in.By, at); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return d, nil
}

// resolveSettlementDispute 按来源 A/B 处理资金：
//   - 来源 A（无 settle 流水）：confirm/adjust 必须补写唯一 settle 并入账；
//     reject 只回 pending，不动钱。
//   - 来源 B（已有 settle 流水）：任何确认/驳回都只释放冻结，绝不重复 settle；
//     adjust 先从冻结额扣减，再释放剩余部分。
func (r *disputeRepository) resolveSettlementDispute(
	ctx context.Context,
	tx *sql.Tx,
	d *service.Dispute,
	in service.ResolveInput,
	at time.Time,
) error {
	var (
		status     string
		merchantID int64
		netPayable float64
		deadline   *time.Time
	)
	err := tx.QueryRowContext(ctx, settlementForDisputeLockQuery, d.TargetID).
		Scan(&status, &merchantID, &netPayable, &deadline)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return service.ErrDisputeTargetNotFound
		}
		return err
	}
	if status != service.SettlementStatusDisputed {
		return service.ErrSettlementConflict
	}

	var hasSettle bool
	if err := tx.QueryRowContext(ctx, settlementHasSettleLedgerQuery, d.TargetID).Scan(&hasSettle); err != nil {
		return err
	}

	switch in.Decision {
	case service.DisputeDecisionReject:
		if hasSettle {
			if err := updateSettlementResolvedConfirmed(ctx, tx, d.TargetID, at); err != nil {
				return err
			}
			return releaseSettlementFreeze(ctx, tx, d, merchantID, netPayable, at)
		}
		res, err := tx.ExecContext(ctx, settlementResolvePendingUpdate,
			d.TargetID, at.UTC().Add(service.SettlementDisputeWindow), at.UTC())
		if err != nil {
			return err
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if affected == 0 {
			return service.ErrSettlementConflict
		}
		return nil

	case service.DisputeDecisionConfirm:
		if err := updateSettlementResolvedConfirmed(ctx, tx, d.TargetID, at); err != nil {
			return err
		}
		if hasSettle {
			return releaseSettlementFreeze(ctx, tx, d, merchantID, netPayable, at)
		}
		return creditSettlementOnce(ctx, tx, d.TargetID, at)

	case service.DisputeDecisionAdjust:
		if err := updateSettlementResolvedConfirmed(ctx, tx, d.TargetID, at); err != nil {
			return err
		}
		if hasSettle {
			return adjustFrozenSettlement(ctx, tx, d, merchantID, netPayable, at)
		}
		if err := creditSettlementOnce(ctx, tx, d.TargetID, at); err != nil {
			return err
		}
		return adjustCreditedMerchant(ctx, tx, d, merchantID, at)
	default:
		return service.ErrDisputeDecisionInvalid
	}
}

func updateSettlementResolvedConfirmed(ctx context.Context, tx *sql.Tx, settlementID int64, at time.Time) error {
	res, err := tx.ExecContext(ctx, settlementResolveConfirmedUpdate, settlementID, at.UTC())
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return service.ErrSettlementConflict
	}
	return nil
}

// creditSettlementOnce 只在 settle 流水真正插入时给余额入账。
// UNIQUE(type, ref_type, ref_id) 是“恰好一次”的最后一道防线。
func creditSettlementOnce(ctx context.Context, tx *sql.Tx, settlementID int64, at time.Time) error {
	res, err := tx.ExecContext(ctx, settlementLedgerInsert, settlementID, at.UTC())
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return nil
	}
	_, err = tx.ExecContext(ctx, settlementBalanceCredit, settlementID, at.UTC())
	return err
}

// releaseSettlementFreeze 释放来源 B 的冻结：frozen -= net, balance += net。
// release 流水以 dispute 为幂等键，重复裁决不会二次释放。
func releaseSettlementFreeze(
	ctx context.Context,
	tx *sql.Tx,
	d *service.Dispute,
	merchantID int64,
	netPayable float64,
	at time.Time,
) error {
	res, err := tx.ExecContext(ctx, disputeReleaseLedgerInsert,
		merchantID, netPayable, d.ID, at.UTC())
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return nil
	}
	_, err = tx.ExecContext(ctx, disputeReleaseBalanceQuery,
		merchantID, netPayable, at.UTC())
	return err
}

// adjustFrozenSettlement 处理来源 B 的调账：
// 先从未冻结额扣除负向调账，再释放剩余冻结额；正向调账在释放后补入余额。
// 流水记 release(+net) 与 adjust(±amount)，两者之和等于 balance 的真实增量。
func adjustFrozenSettlement(
	ctx context.Context,
	tx *sql.Tx,
	d *service.Dispute,
	merchantID int64,
	netPayable float64,
	at time.Time,
) error {
	res, err := tx.ExecContext(ctx, disputeReleaseLedgerInsert,
		merchantID, netPayable, d.ID, at.UTC())
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows > 0 {
		if d.AdjustAmount < 0 {
			deduction := -d.AdjustAmount
			if _, err := tx.ExecContext(ctx, disputeAdjustDeductFrozenQuery,
				merchantID, deduction, at.UTC()); err != nil {
				return err
			}
			remainder := netPayable - deduction
			if _, err := tx.ExecContext(ctx, disputeAdjustReleaseQuery,
				merchantID, remainder, remainder, at.UTC()); err != nil {
				return err
			}
		} else {
			if _, err := tx.ExecContext(ctx, disputeAdjustReleaseQuery,
				merchantID, netPayable, netPayable, at.UTC()); err != nil {
				return err
			}
			if d.AdjustAmount > 0 {
				if _, err := tx.ExecContext(ctx, disputeAdjustCreditQuery,
					merchantID, d.AdjustAmount, at.UTC()); err != nil {
					return err
				}
			}
		}
	}
	return insertDisputeAdjustLedger(ctx, tx, d, merchantID, at)
}

// adjustCreditedMerchant 处理来源 A 的调账：结算净额已按 settle 入账，
// 调账直接作用在可用余额上（来源 A 从未冻结，没有 frozen 可扣）。
func adjustCreditedMerchant(
	ctx context.Context,
	tx *sql.Tx,
	d *service.Dispute,
	merchantID int64,
	at time.Time,
) error {
	if d.AdjustAmount == 0 {
		return nil
	}
	res, err := tx.ExecContext(ctx, disputeAdjustLedgerInsert,
		merchantID, d.AdjustAmount, d.ID, at.UTC())
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return nil
	}
	_, err = tx.ExecContext(ctx, disputeAdjustCreditQuery,
		merchantID, d.AdjustAmount, at.UTC())
	return err
}

// insertDisputeAdjustLedger 只记录调账流水；来源 B 的资金已在释放冻结时处理完，
// 这里再改余额会重复扣减/补足。
func insertDisputeAdjustLedger(
	ctx context.Context,
	tx *sql.Tx,
	d *service.Dispute,
	merchantID int64,
	at time.Time,
) error {
	if d.AdjustAmount == 0 {
		return nil
	}
	_, err := tx.ExecContext(ctx, disputeAdjustLedgerInsert,
		merchantID, d.AdjustAmount, d.ID, at.UTC())
	return err
}

// resolveUsageDispute 只处理用户余额调账；用户侧没有 merchant_ledger。
func resolveUsageDispute(
	ctx context.Context,
	tx *sql.Tx,
	d *service.Dispute,
	in service.ResolveInput,
	at time.Time,
) error {
	if in.Decision != service.DisputeDecisionAdjust || in.AdjustAmount == 0 {
		return nil
	}

	res, err := tx.ExecContext(ctx, disputeUserBalanceQuery,
		in.AdjustAmount, at.UTC(), d.OwnerID)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected > 0 {
		return nil
	}

	var exists int
	err = tx.QueryRowContext(ctx, disputeUserExistsQuery, d.OwnerID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return service.ErrUserNotFound
	}
	if err != nil {
		return err
	}
	return service.ErrBalanceNegative
}

// AdjustLedger 写一条带 dispute 来源的调账流水。
// 没有 dispute 引用直接拒绝；同一 dispute 只能调账一次。
func (r *disputeRepository) AdjustLedger(
	ctx context.Context,
	merchantID int64,
	amount float64,
	refType string,
	refID int64,
	at time.Time,
) error {
	if refType != "dispute" || refID <= 0 {
		return service.ErrAdjustRequiresSource
	}

	tx, err := r.beginTx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var ownerType string
	var ownerID int64
	if err := tx.QueryRowContext(ctx, disputeOwnerQuery, refID).Scan(&ownerType, &ownerID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return service.ErrDisputeNotFound
		}
		return err
	}
	if ownerType != service.DisputeOwnerMerchant || ownerID != merchantID {
		return service.ErrDisputeNotFound
	}

	res, err := tx.ExecContext(ctx, disputeAdjustLedgerInsert,
		merchantID, amount, refID, at.UTC())
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows > 0 {
		if _, err := tx.ExecContext(ctx, disputeAdjustCreditQuery,
			merchantID, amount, at.UTC()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func insertDisputeAudit(
	ctx context.Context,
	tx *sql.Tx,
	action string,
	d *service.Dispute,
	actor string,
	at time.Time,
) error {
	if actor == "" {
		actor = service.DisputeDefaultOperator
	}
	extra, err := json.Marshal(map[string]any{
		"dispute_id":    d.ID,
		"owner_type":    d.OwnerType,
		"owner_id":      d.OwnerID,
		"target_type":   d.TargetType,
		"target_id":     d.TargetID,
		"status":        d.Status,
		"decision":      d.Decision,
		"adjust_amount": d.AdjustAmount,
		"note":          d.Note,
		"at":            at.UTC().Format(time.RFC3339),
	})
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, disputeAuditInsert,
		at.UTC(),
		truncateString(actor, 255),
		action,
		fmt.Sprintf("/disputes/%d", d.ID),
		string(extra),
	)
	return err
}
