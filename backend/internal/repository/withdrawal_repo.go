package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/woolsen/router2api/internal/service"
)

// withdrawalColumns 是 service.Withdrawal 的读取列，顺序必须与 withdrawalScanDest 一致。
// 可空列统一 COALESCE 成空串，避免 database/sql 把 NULL 扫进 string 失败。
const withdrawalColumns = `id, merchant_id, amount, status,
COALESCE(proof_path, '') AS proof_path,
COALESCE(proof_sha256, '') AS proof_sha256,
COALESCE(approved_by, '') AS approved_by,
requested_at, paid_at`

const withdrawalAvailableQuery = `
SELECT balance
FROM merchant_balance
WHERE merchant_id = $1`

// withdrawalEnsureBalanceQuery 保证余额行存在，随后才能 FOR UPDATE 串行化同一商户的提现请求。
// 没有行时 SELECT ... FOR UPDATE 锁不到任何东西，两个并发请求会同时通过。
const withdrawalEnsureBalanceQuery = `
INSERT INTO merchant_balance (merchant_id, balance, frozen, updated_at)
VALUES ($1, 0, 0, $2)
ON CONFLICT (merchant_id) DO NOTHING`

const withdrawalBalanceLockQuery = `
SELECT balance
FROM merchant_balance
WHERE merchant_id = $1
FOR UPDATE`

const withdrawalOpenQuery = `
SELECT id
FROM withdrawals
WHERE merchant_id = $1
  AND status IN ('requested', 'approved')
LIMIT 1`

const withdrawalInsertQuery = `
INSERT INTO withdrawals (merchant_id, amount, status, requested_at)
VALUES ($1, $2, $3, $4)
RETURNING ` + withdrawalColumns

const withdrawalFreezeBalanceQuery = `
UPDATE merchant_balance
SET balance = balance - $2,
    frozen = frozen + $2,
    updated_at = $3
WHERE merchant_id = $1`

const withdrawalListByMerchantQuery = `
SELECT ` + withdrawalColumns + `
FROM withdrawals
WHERE merchant_id = $1
ORDER BY requested_at DESC, id DESC`

const withdrawalListAllQuery = `
SELECT ` + withdrawalColumns + `
FROM withdrawals
WHERE ($1 = '' OR status = $1)
ORDER BY requested_at DESC, id DESC
LIMIT $2`

const withdrawalApproveQuery = `
UPDATE withdrawals
SET status = 'approved',
    approved_by = $2
WHERE id = $1
  AND status = 'requested'`

const withdrawalStatusQuery = `
SELECT status
FROM withdrawals
WHERE id = $1`

const withdrawalRejectLockQuery = `
SELECT merchant_id, amount, status
FROM withdrawals
WHERE id = $1
FOR UPDATE`

const withdrawalRejectUpdate = `
UPDATE withdrawals
SET status = 'rejected',
    approved_by = $2
WHERE id = $1
  AND status IN ('requested', 'approved')`

const withdrawalPayUpdate = `
UPDATE withdrawals
SET status = 'paid',
    proof_path = $2,
    proof_sha256 = $3,
    approved_by = $4,
    paid_at = $5
WHERE id = $1
  AND status = 'approved'
RETURNING merchant_id, amount`

const withdrawalReleaseFrozenQuery = `
UPDATE merchant_balance
SET frozen = frozen - $2,
    updated_at = $3
WHERE merchant_id = $1`

// withdrawalLedgerInsert 是资金流水的唯一写法。
// UNIQUE(type, ref_type, ref_id) 保证同一提现单的同一资金动作只记一次。
const withdrawalLedgerInsert = `
INSERT INTO merchant_ledger (merchant_id, type, amount, ref_type, ref_id, created_at)
VALUES ($1, $2, $3, 'withdrawal', $4, $5)
ON CONFLICT (type, ref_type, ref_id) DO NOTHING`

const withdrawalUnfreezeBalanceQuery = `
UPDATE merchant_balance
SET balance = balance + $2,
    frozen = frozen - $2,
    updated_at = $3
WHERE merchant_id = $1`

type withdrawalRepository struct {
	sql sqlExecutor
	db  sqlBeginner
}

func NewWithdrawalRepository(sqlDB *sql.DB) service.WithdrawalRepository {
	return newWithdrawalRepositoryWithSQL(sqlDB)
}

func newWithdrawalRepositoryWithSQL(sqlq sqlExecutor) *withdrawalRepository {
	repo := &withdrawalRepository{sql: sqlq}
	if beginner, ok := sqlq.(sqlBeginner); ok {
		repo.db = beginner
	}
	return repo
}

func withdrawalScanDest(w *service.Withdrawal) []any {
	return []any{
		&w.ID,
		&w.MerchantID,
		&w.Amount,
		&w.Status,
		&w.ProofPath,
		&w.ProofSHA256,
		&w.ApprovedBy,
		&w.RequestedAt,
		&w.PaidAt,
	}
}

func scanWithdrawalRow(row interface{ Scan(dest ...any) error }) (*service.Withdrawal, error) {
	var w service.Withdrawal
	if err := row.Scan(withdrawalScanDest(&w)...); err != nil {
		return nil, err
	}
	return &w, nil
}

func (r *withdrawalRepository) Available(ctx context.Context, merchantID int64) (float64, error) {
	var balance float64
	err := scanSingleRow(ctx, r.sql, withdrawalAvailableQuery, []any{merchantID}, &balance)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	return balance, nil
}

func (r *withdrawalRepository) List(ctx context.Context, merchantID int64) ([]service.Withdrawal, error) {
	rows, err := r.sql.QueryContext(ctx, withdrawalListByMerchantQuery, merchantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanWithdrawalRows(rows)
}

func (r *withdrawalRepository) ListAll(ctx context.Context, status string) ([]service.Withdrawal, error) {
	rows, err := r.sql.QueryContext(ctx, withdrawalListAllQuery, strings.TrimSpace(status), 200)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanWithdrawalRows(rows)
}

func scanWithdrawalRows(rows *sql.Rows) ([]service.Withdrawal, error) {
	items := make([]service.Withdrawal, 0)
	for rows.Next() {
		w, err := scanWithdrawalRow(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, *w)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

func (r *withdrawalRepository) beginTx(ctx context.Context) (*sql.Tx, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("withdrawal repository is not transactional")
	}
	return r.db.BeginTx(ctx, nil)
}

// Request 在一个事务内完成：
// 1) 确保余额行存在并 FOR UPDATE，锁住该商户的账本；
// 2) 锁后重算可用额并检查在途提现（不能先查后锁）；
// 3) 写提现单、balance -= amount、frozen += amount、写 withdraw 流水。
// 部分唯一索引 idx_withdrawals_open 是单笔在途的最后一道防线。
func (r *withdrawalRepository) Request(
	ctx context.Context,
	merchantID int64,
	amount float64,
	at time.Time,
) (*service.Withdrawal, error) {
	tx, err := r.beginTx(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, withdrawalEnsureBalanceQuery, merchantID, at.UTC()); err != nil {
		return nil, err
	}

	var balance float64
	if err := tx.QueryRowContext(ctx, withdrawalBalanceLockQuery, merchantID).Scan(&balance); err != nil {
		return nil, err
	}
	if balance+1e-9 < amount {
		return nil, service.ErrAmountExceedsAvailable
	}

	var openID int64
	err = tx.QueryRowContext(ctx, withdrawalOpenQuery, merchantID).Scan(&openID)
	if err == nil {
		return nil, service.ErrWithdrawalInFlight
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	w, err := scanWithdrawalRow(tx.QueryRowContext(ctx, withdrawalInsertQuery,
		merchantID, amount, service.WithdrawalStatusRequested, at.UTC()))
	if err != nil {
		if isUniqueConstraintViolation(err) {
			return nil, service.ErrWithdrawalInFlight
		}
		return nil, err
	}

	if _, err := tx.ExecContext(ctx, withdrawalFreezeBalanceQuery, merchantID, amount, at.UTC()); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, withdrawalLedgerInsert,
		merchantID, "withdraw", -amount, w.ID, at.UTC()); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return w, nil
}

// Approve 只改状态，不动钱；资金在 Request 时已进入 frozen。
func (r *withdrawalRepository) Approve(ctx context.Context, withdrawalID int64, by string, _ time.Time) error {
	res, err := r.sql.ExecContext(ctx, withdrawalApproveQuery, withdrawalID, by)
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

	status, err := r.withdrawalStatus(ctx, withdrawalID)
	if err != nil {
		return err
	}
	switch status {
	case service.WithdrawalStatusApproved:
		return nil
	case service.WithdrawalStatusPaid:
		return service.ErrAlreadyPaid
	default:
		return service.ErrWithdrawalStatusTransition
	}
}

func (r *withdrawalRepository) withdrawalStatus(ctx context.Context, withdrawalID int64) (string, error) {
	var status string
	if err := scanSingleRow(ctx, r.sql, withdrawalStatusQuery, []any{withdrawalID}, &status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", service.ErrWithdrawalNotFound
		}
		return "", err
	}
	return status, nil
}

// Reject 在行锁内完成状态流转与解冻：
// balance += amount、frozen -= amount，并写 withdraw_reject 流水。
// 流水唯一键命中时不重复退款，重复拒绝同一笔是幂等的。
func (r *withdrawalRepository) Reject(ctx context.Context, withdrawalID int64, by string, at time.Time) error {
	tx, err := r.beginTx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var (
		merchantID int64
		amount     float64
		status     string
	)
	err = tx.QueryRowContext(ctx, withdrawalRejectLockQuery, withdrawalID).
		Scan(&merchantID, &amount, &status)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return service.ErrWithdrawalNotFound
		}
		return err
	}
	switch status {
	case service.WithdrawalStatusRejected:
		return nil
	case service.WithdrawalStatusPaid:
		return service.ErrAlreadyPaid
	case service.WithdrawalStatusRequested, service.WithdrawalStatusApproved:
	default:
		return service.ErrWithdrawalStatusTransition
	}

	res, err := tx.ExecContext(ctx, withdrawalRejectUpdate, withdrawalID, by)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return service.ErrWithdrawalStatusTransition
	}

	ledgerRes, err := tx.ExecContext(ctx, withdrawalLedgerInsert,
		merchantID, "withdraw_reject", amount, withdrawalID, at.UTC())
	if err != nil {
		return err
	}
	ledgerRows, err := ledgerRes.RowsAffected()
	if err != nil {
		return err
	}
	if ledgerRows > 0 {
		if _, err := tx.ExecContext(ctx, withdrawalUnfreezeBalanceQuery, merchantID, amount, at.UTC()); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// MarkPaid 用条件更新 WHERE status='approved' 完成打款：
// 0 行说明已打款或尚未审核，必须返回 ErrAlreadyPaid，不能幂等返回 nil。
// 成功后只释放 frozen（balance 在 Request 时已扣除），并写 withdraw_paid 流水。
func (r *withdrawalRepository) MarkPaid(
	ctx context.Context,
	withdrawalID int64,
	proofPath, sha256, by string,
	at time.Time,
) error {
	tx, err := r.beginTx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var (
		merchantID int64
		amount     float64
	)
	err = tx.QueryRowContext(ctx, withdrawalPayUpdate,
		withdrawalID, proofPath, sha256, by, at.UTC()).
		Scan(&merchantID, &amount)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			var status string
			statusErr := tx.QueryRowContext(ctx, withdrawalStatusQuery, withdrawalID).Scan(&status)
			if errors.Is(statusErr, sql.ErrNoRows) {
				return service.ErrWithdrawalNotFound
			}
			if statusErr != nil {
				return statusErr
			}
			return service.ErrAlreadyPaid
		}
		return err
	}

	if _, err := tx.ExecContext(ctx, withdrawalReleaseFrozenQuery, merchantID, amount, at.UTC()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, withdrawalLedgerInsert,
		merchantID, "withdraw_paid", 0.0, withdrawalID, at.UTC()); err != nil {
		return err
	}

	return tx.Commit()
}
