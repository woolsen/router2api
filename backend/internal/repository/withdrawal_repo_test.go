package repository

import (
	"context"
	"database/sql"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"

	"github.com/woolsen/router2api/internal/service"
)

func withdrawalRowColumns() []string {
	return []string{
		"id", "merchant_id", "amount", "status",
		"proof_path", "proof_sha256", "approved_by", "requested_at", "paid_at",
	}
}

func TestWithdrawalRepositoryAvailableReadsBalanceOnly(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newWithdrawalRepositoryWithSQL(db)

	// 只允许一条 merchant_balance 查询：pending 结算不会出现在这个口径里。
	mock.ExpectQuery(regexp.QuoteMeta(withdrawalAvailableQuery)).
		WithArgs(int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"balance"}).AddRow(100.0))

	got, err := repo.Available(context.Background(), 7)
	require.NoError(t, err)
	require.InDelta(t, 100, got, 1e-9)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestWithdrawalRepositoryAvailableMissingRowIsZero(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newWithdrawalRepositoryWithSQL(db)

	mock.ExpectQuery(regexp.QuoteMeta(withdrawalAvailableQuery)).
		WithArgs(int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"balance"}))

	got, err := repo.Available(context.Background(), 7)
	require.NoError(t, err)
	require.Zero(t, got)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestWithdrawalRepositoryRequestFreezesAndWritesLedger(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newWithdrawalRepositoryWithSQL(db)
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(withdrawalEnsureBalanceQuery)).
		WithArgs(int64(7), at).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta(withdrawalBalanceLockQuery)).
		WithArgs(int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"balance"}).AddRow(100.0))
	mock.ExpectQuery(regexp.QuoteMeta(withdrawalOpenQuery)).
		WithArgs(int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectQuery(regexp.QuoteMeta(withdrawalInsertQuery)).
		WithArgs(int64(7), 60.0, service.WithdrawalStatusRequested, at).
		WillReturnRows(sqlmock.NewRows(withdrawalRowColumns()).
			AddRow(int64(11), int64(7), 60.0, service.WithdrawalStatusRequested, "", "", "", at, nil))
	mock.ExpectExec(regexp.QuoteMeta(withdrawalFreezeBalanceQuery)).
		WithArgs(int64(7), 60.0, at).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(withdrawalLedgerInsert)).
		WithArgs(int64(7), "withdraw", -60.0, int64(11), at).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	w, err := repo.Request(context.Background(), 7, 60, at)
	require.NoError(t, err)
	require.Equal(t, int64(11), w.ID)
	require.Equal(t, service.WithdrawalStatusRequested, w.Status)
	require.InDelta(t, 60, w.Amount, 1e-9)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestWithdrawalRepositoryRequestRejectsInsufficientBalance(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newWithdrawalRepositoryWithSQL(db)
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(withdrawalEnsureBalanceQuery)).
		WithArgs(int64(7), at).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta(withdrawalBalanceLockQuery)).
		WithArgs(int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"balance"}).AddRow(40.0))
	mock.ExpectRollback()

	_, err := repo.Request(context.Background(), 7, 60, at)
	require.ErrorIs(t, err, service.ErrAmountExceedsAvailable)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestWithdrawalRepositoryRequestRejectsSecondInFlight(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newWithdrawalRepositoryWithSQL(db)
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(withdrawalEnsureBalanceQuery)).
		WithArgs(int64(7), at).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta(withdrawalBalanceLockQuery)).
		WithArgs(int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"balance"}).AddRow(200.0))
	mock.ExpectQuery(regexp.QuoteMeta(withdrawalOpenQuery)).
		WithArgs(int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(9)))
	mock.ExpectRollback()

	_, err := repo.Request(context.Background(), 7, 60, at)
	require.ErrorIs(t, err, service.ErrWithdrawalInFlight)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestWithdrawalRepositoryRequestUniqueViolationIsInFlight(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newWithdrawalRepositoryWithSQL(db)
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(withdrawalEnsureBalanceQuery)).
		WithArgs(int64(7), at).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta(withdrawalBalanceLockQuery)).
		WithArgs(int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"balance"}).AddRow(100.0))
	mock.ExpectQuery(regexp.QuoteMeta(withdrawalOpenQuery)).
		WithArgs(int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectQuery(regexp.QuoteMeta(withdrawalInsertQuery)).
		WithArgs(int64(7), 60.0, service.WithdrawalStatusRequested, at).
		WillReturnError(&pq.Error{Code: "23505"})
	mock.ExpectRollback()

	_, err := repo.Request(context.Background(), 7, 60, at)
	require.ErrorIs(t, err, service.ErrWithdrawalInFlight)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestWithdrawalRepositoryApproveIsConditional(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newWithdrawalRepositoryWithSQL(db)

	mock.ExpectExec(regexp.QuoteMeta(withdrawalApproveQuery)).
		WithArgs(int64(11), "ops@example.com").
		WillReturnResult(sqlmock.NewResult(0, 1))

	require.NoError(t, repo.Approve(context.Background(), 11, "ops@example.com", time.Now()))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestWithdrawalRepositoryApproveAlreadyApprovedIsIdempotent(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newWithdrawalRepositoryWithSQL(db)

	mock.ExpectExec(regexp.QuoteMeta(withdrawalApproveQuery)).
		WithArgs(int64(11), "ops").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta(withdrawalStatusQuery)).
		WithArgs(int64(11)).
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow(service.WithdrawalStatusApproved))

	require.NoError(t, repo.Approve(context.Background(), 11, "ops", time.Now()))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestWithdrawalRepositoryApproveRejectsPaid(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newWithdrawalRepositoryWithSQL(db)

	mock.ExpectExec(regexp.QuoteMeta(withdrawalApproveQuery)).
		WithArgs(int64(11), "ops").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta(withdrawalStatusQuery)).
		WithArgs(int64(11)).
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow(service.WithdrawalStatusPaid))

	err := repo.Approve(context.Background(), 11, "ops", time.Now())
	require.ErrorIs(t, err, service.ErrAlreadyPaid)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestWithdrawalRepositoryMarkPaidReleasesFrozen(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newWithdrawalRepositoryWithSQL(db)
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(withdrawalPayUpdate)).
		WithArgs(int64(11), "proof.png", "sha", "ops", at).
		WillReturnRows(sqlmock.NewRows([]string{"merchant_id", "amount"}).AddRow(int64(7), 60.0))
	mock.ExpectExec(regexp.QuoteMeta(withdrawalReleaseFrozenQuery)).
		WithArgs(int64(7), 60.0, at).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(withdrawalLedgerInsert)).
		WithArgs(int64(7), "withdraw_paid", 0.0, int64(11), at).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	require.NoError(t, repo.MarkPaid(context.Background(), 11, "proof.png", "sha", "ops", at))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestWithdrawalRepositoryMarkPaidTwiceReturnsAlreadyPaid(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newWithdrawalRepositoryWithSQL(db)
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(withdrawalPayUpdate)).
		WithArgs(int64(11), "proof.png", "sha", "ops", at).
		WillReturnRows(sqlmock.NewRows([]string{"merchant_id", "amount"}))
	mock.ExpectQuery(regexp.QuoteMeta(withdrawalStatusQuery)).
		WithArgs(int64(11)).
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow(service.WithdrawalStatusPaid))
	mock.ExpectRollback()

	err := repo.MarkPaid(context.Background(), 11, "proof.png", "sha", "ops", at)
	require.ErrorIs(t, err, service.ErrAlreadyPaid)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestWithdrawalRepositoryMarkPaidMissingIsNotFound(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newWithdrawalRepositoryWithSQL(db)
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(withdrawalPayUpdate)).
		WithArgs(int64(11), "proof.png", "sha", "ops", at).
		WillReturnRows(sqlmock.NewRows([]string{"merchant_id", "amount"}))
	mock.ExpectQuery(regexp.QuoteMeta(withdrawalStatusQuery)).
		WithArgs(int64(11)).
		WillReturnRows(sqlmock.NewRows([]string{"status"}))
	mock.ExpectRollback()

	err := repo.MarkPaid(context.Background(), 11, "proof.png", "sha", "ops", at)
	require.ErrorIs(t, err, service.ErrWithdrawalNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestWithdrawalRepositoryRejectUnfreezes(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newWithdrawalRepositoryWithSQL(db)
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(withdrawalRejectLockQuery)).
		WithArgs(int64(11)).
		WillReturnRows(sqlmock.NewRows([]string{"merchant_id", "amount", "status"}).
			AddRow(int64(7), 60.0, service.WithdrawalStatusRequested))
	mock.ExpectExec(regexp.QuoteMeta(withdrawalRejectUpdate)).
		WithArgs(int64(11), "ops").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(withdrawalLedgerInsert)).
		WithArgs(int64(7), "withdraw_reject", 60.0, int64(11), at).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(regexp.QuoteMeta(withdrawalUnfreezeBalanceQuery)).
		WithArgs(int64(7), 60.0, at).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	require.NoError(t, repo.Reject(context.Background(), 11, "ops", at))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestWithdrawalRepositoryRejectAlreadyRejectedIsIdempotent(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newWithdrawalRepositoryWithSQL(db)
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(withdrawalRejectLockQuery)).
		WithArgs(int64(11)).
		WillReturnRows(sqlmock.NewRows([]string{"merchant_id", "amount", "status"}).
			AddRow(int64(7), 60.0, service.WithdrawalStatusRejected))
	mock.ExpectRollback()

	require.NoError(t, repo.Reject(context.Background(), 11, "ops", at))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestWithdrawalRepositoryRejectLedgerConflictDoesNotUnfreezeAgain(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newWithdrawalRepositoryWithSQL(db)
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(withdrawalRejectLockQuery)).
		WithArgs(int64(11)).
		WillReturnRows(sqlmock.NewRows([]string{"merchant_id", "amount", "status"}).
			AddRow(int64(7), 60.0, service.WithdrawalStatusRequested))
	mock.ExpectExec(regexp.QuoteMeta(withdrawalRejectUpdate)).
		WithArgs(int64(11), "ops").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(withdrawalLedgerInsert)).
		WithArgs(int64(7), "withdraw_reject", 60.0, int64(11), at).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()

	require.NoError(t, repo.Reject(context.Background(), 11, "ops", at))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestWithdrawalRepositoryRejectPaidFails(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newWithdrawalRepositoryWithSQL(db)
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(withdrawalRejectLockQuery)).
		WithArgs(int64(11)).
		WillReturnRows(sqlmock.NewRows([]string{"merchant_id", "amount", "status"}).
			AddRow(int64(7), 60.0, service.WithdrawalStatusPaid))
	mock.ExpectRollback()

	err := repo.Reject(context.Background(), 11, "ops", at)
	require.ErrorIs(t, err, service.ErrAlreadyPaid)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestWithdrawalRepositoryListScansNullableColumns(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newWithdrawalRepositoryWithSQL(db)
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

	mock.ExpectQuery(regexp.QuoteMeta(withdrawalListByMerchantQuery)).
		WithArgs(int64(7)).
		WillReturnRows(sqlmock.NewRows(withdrawalRowColumns()).
			AddRow(int64(11), int64(7), 60.0, service.WithdrawalStatusPaid, "proof.png", "sha", "ops", at, at))

	items, err := repo.List(context.Background(), 7)
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.Equal(t, "proof.png", items[0].ProofPath)
	require.NotNil(t, items[0].PaidAt)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestWithdrawalRepositoryListAllFiltersByStatus(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newWithdrawalRepositoryWithSQL(db)

	mock.ExpectQuery(regexp.QuoteMeta(withdrawalListAllQuery)).
		WithArgs(service.WithdrawalStatusRequested, 200).
		WillReturnRows(sqlmock.NewRows(withdrawalRowColumns()))

	items, err := repo.ListAll(context.Background(), service.WithdrawalStatusRequested)
	require.NoError(t, err)
	require.Empty(t, items)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestWithdrawalRepositoryIsNotTransactionalWithoutDB(t *testing.T) {
	repo := newWithdrawalRepositoryWithSQL(nil)
	_, err := repo.Request(context.Background(), 7, 60, time.Now())
	require.Error(t, err)
	require.NotErrorIs(t, err, sql.ErrNoRows)
}
