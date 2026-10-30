package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	dbent "github.com/woolsen/router2api/ent"
	"github.com/woolsen/router2api/internal/service"
)

const settlementGetQuery = `
SELECT ` + settlementColumns + `
FROM settlements
WHERE id = $1`

const settlementExpiredPendingQuery = `
SELECT ` + settlementColumns + `
FROM settlements
WHERE status = $1
  AND dispute_deadline < $2
  AND id > $3
ORDER BY id
LIMIT $4`

const settlementLockQuery = `
SELECT status
FROM settlements
WHERE id = $1
FOR UPDATE`

const settlementDisputeLockQuery = `
SELECT status, dispute_deadline, merchant_id, net_payable
FROM settlements
WHERE id = $1
FOR UPDATE`

const settlementConfirmUpdate = `
UPDATE settlements
SET status = $2,
    confirmed_at = $3,
    updated_at = $3
WHERE id = $1
  AND status = $4`

const settlementDisputeUpdate = `
UPDATE settlements
SET status = $2,
    updated_at = $3
WHERE id = $1
  AND status IN ($4, $5)
  AND (dispute_deadline IS NULL OR dispute_deadline > $3)`

// settlementLedgerInsert 只在首次入账时返回 1 行；
// 同一结算单重复确认会命中 UNIQUE(type, ref_type, ref_id) 而不重复加余额。
const settlementLedgerInsert = `
INSERT INTO merchant_ledger (merchant_id, type, amount, ref_type, ref_id, created_at)
SELECT merchant_id, 'settle', net_payable, 'settlement', id, $2
FROM settlements
WHERE id = $1
ON CONFLICT (type, ref_type, ref_id) DO NOTHING`

const settlementBalanceCredit = `
INSERT INTO merchant_balance (merchant_id, balance, frozen, updated_at)
SELECT merchant_id, net_payable, 0, $2
FROM settlements
WHERE id = $1
ON CONFLICT (merchant_id) DO UPDATE
SET balance = merchant_balance.balance + EXCLUDED.balance,
    updated_at = EXCLUDED.updated_at`

// settlementDisputeHoldLedgerInsert 记录申诉冻结：已入账余额从可用额度转入冻结。
// amount 记为 -net_payable（与 settle 的 +net_payable 相反）；
// UNIQUE(type, ref_type, ref_id) 保证同一结算单只冻结一次。
const settlementDisputeHoldLedgerInsert = `
INSERT INTO merchant_ledger (merchant_id, type, amount, ref_type, ref_id, created_at)
SELECT merchant_id, 'dispute_hold', -net_payable, 'settlement', id, $2
FROM settlements
WHERE id = $1
ON CONFLICT (type, ref_type, ref_id) DO NOTHING`

// settlementDisputeHoldBalance 把已入账金额从 balance 移到 frozen。
// 金额直接取 settlements.net_payable，避免经过 Go 的 float64 再写回；
// 冲突分支里的 EXCLUDED.frozen 就是本次要冻结的 net_payable。
// 不检查余额是否足够：商户可能已经提现，此时允许 balance 为负，
// 由调用方记录日志交运营人工处理，不能静默跳过冻结。
const settlementDisputeHoldBalance = `
INSERT INTO merchant_balance (merchant_id, balance, frozen, updated_at)
SELECT merchant_id, -net_payable, net_payable, $2
FROM settlements
WHERE id = $1
ON CONFLICT (merchant_id) DO UPDATE
SET balance = merchant_balance.balance - EXCLUDED.frozen,
    frozen = merchant_balance.frozen + EXCLUDED.frozen,
    updated_at = EXCLUDED.updated_at
RETURNING balance`

const settlementAuditInsert = `
INSERT INTO audit_logs (
    created_at, actor_email, actor_role, action, method, path, request_body, extra
) VALUES ($1, $2, 'system', $3, 'SYSTEM', $4, '', $5::jsonb)`

func NewSettlementOpsRepository(client *dbent.Client, sqlDB *sql.DB) service.SettlementOpsRepository {
	return newSettlementRepositoryWithSQL(client, sqlDB)
}

func settlementScanDest(s *service.Settlement, createdAt, updatedAt *time.Time) []any {
	return []any{
		&s.ID,
		&s.MerchantID,
		&s.PeriodStart,
		&s.PeriodEnd,
		&s.GrossCost,
		&s.Commission,
		&s.NetPayable,
		&s.Status,
		createdAt,
		updatedAt,
		&s.ConfirmedAt,
		&s.PaidAt,
		&s.DisputeDeadline,
	}
}

func (r *settlementRepository) GetSettlement(ctx context.Context, settlementID int64) (*service.Settlement, error) {
	var s service.Settlement
	var createdAt, updatedAt time.Time
	err := scanSingleRow(ctx, r.sql, settlementGetQuery, []any{settlementID},
		settlementScanDest(&s, &createdAt, &updatedAt)...)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, service.ErrSettlementNotFound
		}
		return nil, err
	}
	return &s, nil
}

func (r *settlementRepository) ListSettlementItems(ctx context.Context, settlementID int64) ([]service.SettlementItem, error) {
	rows, err := r.sql.QueryContext(ctx, `
SELECT id, settlement_id, usage_log_id, cost, commission
FROM settlement_items
WHERE settlement_id = $1
ORDER BY id`, settlementID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	items := make([]service.SettlementItem, 0)
	for rows.Next() {
		var item service.SettlementItem
		if err := rows.Scan(&item.ID, &item.SettlementID, &item.UsageLogID, &item.Cost, &item.Commission); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

func (r *settlementRepository) ListExpiredPendingSettlements(
	ctx context.Context,
	before time.Time,
	afterID int64,
	limit int,
) ([]*service.Settlement, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := r.sql.QueryContext(ctx, settlementExpiredPendingQuery,
		service.SettlementStatusPending, before.UTC(), afterID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	pending := make([]*service.Settlement, 0)
	for rows.Next() {
		var s service.Settlement
		var createdAt, updatedAt time.Time
		if err := rows.Scan(settlementScanDest(&s, &createdAt, &updatedAt)...); err != nil {
			return nil, err
		}
		pending = append(pending, &s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return pending, nil
}

// ConfirmSettlement 在同一事务内完成条件更新与余额入账：
// 1) pending → confirmed（WHERE status='pending'）；
// 2) 写 merchant_ledger；UNIQUE(type,ref_type,ref_id) 是幂等最后防线；
// 3) 只有 ledger 真正插入时才加 merchant_balance.balance。
func (r *settlementRepository) ConfirmSettlement(
	ctx context.Context,
	settlementID int64,
	by string,
	at time.Time,
) (service.SettlementTransition, error) {
	tx, err := r.beginSettlementTx(ctx)
	if err != nil {
		return service.SettlementTransition{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var status string
	err = tx.QueryRowContext(ctx, settlementLockQuery, settlementID).Scan(&status)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return service.SettlementTransition{}, service.ErrSettlementNotFound
		}
		return service.SettlementTransition{}, err
	}
	if status != service.SettlementStatusPending {
		return service.SettlementTransition{Applied: false, Status: status}, nil
	}

	res, err := tx.ExecContext(ctx, settlementConfirmUpdate,
		settlementID, service.SettlementStatusConfirmed, at.UTC(), service.SettlementStatusPending)
	if err != nil {
		return service.SettlementTransition{}, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return service.SettlementTransition{}, err
	}
	if affected == 0 {
		return service.SettlementTransition{}, service.ErrSettlementConflict
	}

	ledgerRes, err := tx.ExecContext(ctx, settlementLedgerInsert, settlementID, at.UTC())
	if err != nil {
		return service.SettlementTransition{}, err
	}
	ledgerRows, err := ledgerRes.RowsAffected()
	if err != nil {
		return service.SettlementTransition{}, err
	}
	if ledgerRows > 0 {
		if _, err := tx.ExecContext(ctx, settlementBalanceCredit, settlementID, at.UTC()); err != nil {
			return service.SettlementTransition{}, err
		}
	}

	if err := insertSettlementAudit(ctx, tx, settlementID, "settlement.confirmed", by, "", at); err != nil {
		return service.SettlementTransition{}, err
	}
	if err := tx.Commit(); err != nil {
		return service.SettlementTransition{}, err
	}
	return service.SettlementTransition{Applied: true, Status: service.SettlementStatusConfirmed}, nil
}

// DisputeSettlement 用条件更新完成 pending|confirmed → disputed，
// 并在 SQL 层再次检查 dispute_deadline，避免调用方时钟与数据库判断不一致。
// confirmed 来源已在确认时入账，必须在同一事务内把净额从 balance 转入 frozen；
// pending 来源从未入账，不冻结。
func (r *settlementRepository) DisputeSettlement(
	ctx context.Context,
	settlementID int64,
	reason string,
	at time.Time,
) (service.SettlementTransition, error) {
	tx, err := r.beginSettlementTx(ctx)
	if err != nil {
		return service.SettlementTransition{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var (
		status     string
		deadline   *time.Time
		merchantID int64
		netPayable float64
	)
	err = tx.QueryRowContext(ctx, settlementDisputeLockQuery, settlementID).
		Scan(&status, &deadline, &merchantID, &netPayable)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return service.SettlementTransition{}, service.ErrSettlementNotFound
		}
		return service.SettlementTransition{}, err
	}
	if status != service.SettlementStatusPending && status != service.SettlementStatusConfirmed {
		return service.SettlementTransition{Applied: false, Status: status}, nil
	}
	if deadline != nil && !at.UTC().Before(*deadline) {
		return service.SettlementTransition{
			Applied:             false,
			Status:              status,
			DisputeWindowClosed: true,
		}, nil
	}

	res, err := tx.ExecContext(ctx, settlementDisputeUpdate,
		settlementID, service.SettlementStatusDisputed, at.UTC(),
		service.SettlementStatusPending, service.SettlementStatusConfirmed)
	if err != nil {
		return service.SettlementTransition{}, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return service.SettlementTransition{}, err
	}
	if affected == 0 {
		return service.SettlementTransition{}, service.ErrSettlementConflict
	}

	// pending → disputed 从未入账，不冻结；
	// confirmed → disputed 已入账，必须同事务把 net_payable 从可用余额转入冻结。
	if status == service.SettlementStatusConfirmed {
		if err := holdDisputedSettlementBalance(ctx, tx, settlementID, merchantID, netPayable, at); err != nil {
			return service.SettlementTransition{}, err
		}
	}

	if err := insertSettlementAudit(ctx, tx, settlementID, "settlement.disputed", service.SettlementDisputeActor, reason, at); err != nil {
		return service.SettlementTransition{}, err
	}
	if err := tx.Commit(); err != nil {
		return service.SettlementTransition{}, err
	}
	return service.SettlementTransition{Applied: true, Status: service.SettlementStatusDisputed}, nil
}

// holdDisputedSettlementBalance 在申诉事务内冻结已入账的结算净额。
// 先写 dispute_hold 流水，只有流水真正插入时才调整余额；唯一键命中说明已经冻结过。
func holdDisputedSettlementBalance(
	ctx context.Context,
	tx *sql.Tx,
	settlementID, merchantID int64,
	netPayable float64,
	at time.Time,
) error {
	res, err := tx.ExecContext(ctx, settlementDisputeHoldLedgerInsert, settlementID, at.UTC())
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
			"[Settlement] dispute hold overdrew merchant %d: settlement %d net %.10f, balance after hold %.10f < 0; the merchant may already have withdrawn these funds, manual reconciliation required",
			merchantID, settlementID, netPayable, balanceAfter)
	}
	return nil
}

func (r *settlementRepository) beginSettlementTx(ctx context.Context) (*sql.Tx, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("settlement repository is not transactional")
	}
	return r.db.BeginTx(ctx, nil)
}

func insertSettlementAudit(
	ctx context.Context,
	tx *sql.Tx,
	settlementID int64,
	action, actor, reason string,
	at time.Time,
) error {
	if actor == "" {
		actor = service.SettlementDefaultOperator
	}
	extra, err := json.Marshal(map[string]any{
		"settlement_id": settlementID,
		"actor":         actor,
		"reason":        reason,
		"at":            at.UTC().Format(time.RFC3339),
	})
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, settlementAuditInsert,
		at.UTC(),
		truncateString(actor, 255),
		action,
		fmt.Sprintf("/settlements/%d", settlementID),
		string(extra),
	)
	return err
}
