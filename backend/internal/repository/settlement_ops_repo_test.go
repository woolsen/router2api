package repository

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"

	"github.com/woolsen/router2api/internal/service"
)

func TestSettlementRepositoryConfirmMarksAndCreditsOnce(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newSettlementRepositoryWithSQL(nil, db)
	at := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(settlementLockQuery)).
		WithArgs(int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow(service.SettlementStatusPending))
	mock.ExpectExec(regexp.QuoteMeta(settlementConfirmUpdate)).
		WithArgs(int64(7), service.SettlementStatusConfirmed, at, service.SettlementStatusPending).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(settlementLedgerInsert)).
		WithArgs(int64(7), at).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(settlementBalanceCredit)).
		WithArgs(int64(7), at).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(settlementAuditInsert)).
		WithArgs(at, "ops@example.com", "settlement.confirmed", "/settlements/7", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	tr, err := repo.ConfirmSettlement(context.Background(), 7, "ops@example.com", at)
	require.NoError(t, err)
	require.True(t, tr.Applied)
	require.Equal(t, service.SettlementStatusConfirmed, tr.Status)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSettlementRepositoryConfirmDisputedDoesNotTouchBalance(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newSettlementRepositoryWithSQL(nil, db)
	at := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(settlementLockQuery)).
		WithArgs(int64(8)).
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow(service.SettlementStatusDisputed))
	mock.ExpectRollback()

	tr, err := repo.ConfirmSettlement(context.Background(), 8, "ops", at)
	require.NoError(t, err)
	require.False(t, tr.Applied)
	require.Equal(t, service.SettlementStatusDisputed, tr.Status)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSettlementRepositoryConfirmLedgerConflictDoesNotCreditAgain(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newSettlementRepositoryWithSQL(nil, db)
	at := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(settlementLockQuery)).
		WithArgs(int64(9)).
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow(service.SettlementStatusPending))
	mock.ExpectExec(regexp.QuoteMeta(settlementConfirmUpdate)).
		WithArgs(int64(9), service.SettlementStatusConfirmed, at, service.SettlementStatusPending).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// 唯一键已存在：事务不插入余额。
	mock.ExpectExec(regexp.QuoteMeta(settlementLedgerInsert)).
		WithArgs(int64(9), at).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(settlementAuditInsert)).
		WithArgs(at, "auto", "settlement.confirmed", "/settlements/9", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	tr, err := repo.ConfirmSettlement(context.Background(), 9, "auto", at)
	require.NoError(t, err)
	require.True(t, tr.Applied)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSettlementRepositoryConfirmMissingSettlement(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newSettlementRepositoryWithSQL(nil, db)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(settlementLockQuery)).
		WithArgs(int64(10)).
		WillReturnRows(sqlmock.NewRows([]string{"status"}))
	mock.ExpectRollback()

	_, err := repo.ConfirmSettlement(context.Background(), 10, "ops", time.Now())
	require.ErrorIs(t, err, service.ErrSettlementNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSettlementRepositoryDisputeRejectsClosedWindow(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newSettlementRepositoryWithSQL(nil, db)
	at := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	deadline := at.Add(-time.Minute)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(settlementDisputeLockQuery)).
		WithArgs(int64(11)).
		WillReturnRows(sqlmock.NewRows([]string{"status", "dispute_deadline", "merchant_id", "net_payable"}).
			AddRow(service.SettlementStatusPending, deadline, int64(1), 42.0))
	mock.ExpectRollback()

	tr, err := repo.DisputeSettlement(context.Background(), 11, "late", at)
	require.NoError(t, err)
	require.False(t, tr.Applied)
	require.True(t, tr.DisputeWindowClosed)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSettlementRepositoryDisputeMovesPendingToDisputed(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newSettlementRepositoryWithSQL(nil, db)
	at := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	deadline := at.Add(time.Hour)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(settlementDisputeLockQuery)).
		WithArgs(int64(12)).
		WillReturnRows(sqlmock.NewRows([]string{"status", "dispute_deadline", "merchant_id", "net_payable"}).
			AddRow(service.SettlementStatusPending, deadline, int64(1), 42.0))
	mock.ExpectExec(regexp.QuoteMeta(settlementDisputeUpdate)).
		WithArgs(int64(12), service.SettlementStatusDisputed, at,
			service.SettlementStatusPending, service.SettlementStatusConfirmed).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(settlementAuditInsert)).
		WithArgs(at, service.SettlementDisputeActor, "settlement.disputed", "/settlements/12", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	tr, err := repo.DisputeSettlement(context.Background(), 12, "amount mismatch", at)
	require.NoError(t, err)
	require.True(t, tr.Applied)
	require.Equal(t, service.SettlementStatusDisputed, tr.Status)
	require.NoError(t, mock.ExpectationsWereMet())
}

// 已 confirmed 的结算单被申诉时，必须在同一事务内把净额从 balance 转入 frozen。
func TestSettlementRepositoryDisputeConfirmedFreezesCreditedBalance(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newSettlementRepositoryWithSQL(nil, db)
	at := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	deadline := at.Add(time.Hour)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(settlementDisputeLockQuery)).
		WithArgs(int64(14)).
		WillReturnRows(sqlmock.NewRows([]string{"status", "dispute_deadline", "merchant_id", "net_payable"}).
			AddRow(service.SettlementStatusConfirmed, deadline, int64(7), 30.5))
	mock.ExpectExec(regexp.QuoteMeta(settlementDisputeUpdate)).
		WithArgs(int64(14), service.SettlementStatusDisputed, at,
			service.SettlementStatusPending, service.SettlementStatusConfirmed).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(settlementDisputeHoldLedgerInsert)).
		WithArgs(int64(14), at).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(settlementDisputeHoldBalance)).
		WithArgs(int64(14), at).
		WillReturnRows(sqlmock.NewRows([]string{"balance"}).AddRow(19.5))
	mock.ExpectExec(regexp.QuoteMeta(settlementAuditInsert)).
		WithArgs(at, service.SettlementDisputeActor, "settlement.disputed", "/settlements/14", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	tr, err := repo.DisputeSettlement(context.Background(), 14, "amount mismatch", at)
	require.NoError(t, err)
	require.True(t, tr.Applied)
	require.Equal(t, service.SettlementStatusDisputed, tr.Status)
	require.NoError(t, mock.ExpectationsWereMet())
}

// 已提现导致余额不足时也要落冻结（balance 允许为负），不能跳过。
func TestSettlementRepositoryDisputeConfirmedAllowsNegativeBalance(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newSettlementRepositoryWithSQL(nil, db)
	at := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	deadline := at.Add(time.Hour)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(settlementDisputeLockQuery)).
		WithArgs(int64(15)).
		WillReturnRows(sqlmock.NewRows([]string{"status", "dispute_deadline", "merchant_id", "net_payable"}).
			AddRow(service.SettlementStatusConfirmed, deadline, int64(7), 30.5))
	mock.ExpectExec(regexp.QuoteMeta(settlementDisputeUpdate)).
		WithArgs(int64(15), service.SettlementStatusDisputed, at,
			service.SettlementStatusPending, service.SettlementStatusConfirmed).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(settlementDisputeHoldLedgerInsert)).
		WithArgs(int64(15), at).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(settlementDisputeHoldBalance)).
		WithArgs(int64(15), at).
		WillReturnRows(sqlmock.NewRows([]string{"balance"}).AddRow(-10.5))
	mock.ExpectExec(regexp.QuoteMeta(settlementAuditInsert)).
		WithArgs(at, service.SettlementDisputeActor, "settlement.disputed", "/settlements/15", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	tr, err := repo.DisputeSettlement(context.Background(), 15, "amount mismatch", at)
	require.NoError(t, err)
	require.True(t, tr.Applied)
	require.NoError(t, mock.ExpectationsWereMet())
}

// 同一结算单已经冻结过（唯一键命中）时，不再重复扣减余额。
func TestSettlementRepositoryDisputeConfirmedLedgerConflictDoesNotFreezeAgain(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newSettlementRepositoryWithSQL(nil, db)
	at := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	deadline := at.Add(time.Hour)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(settlementDisputeLockQuery)).
		WithArgs(int64(16)).
		WillReturnRows(sqlmock.NewRows([]string{"status", "dispute_deadline", "merchant_id", "net_payable"}).
			AddRow(service.SettlementStatusConfirmed, deadline, int64(7), 30.5))
	mock.ExpectExec(regexp.QuoteMeta(settlementDisputeUpdate)).
		WithArgs(int64(16), service.SettlementStatusDisputed, at,
			service.SettlementStatusPending, service.SettlementStatusConfirmed).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(settlementDisputeHoldLedgerInsert)).
		WithArgs(int64(16), at).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(settlementAuditInsert)).
		WithArgs(at, service.SettlementDisputeActor, "settlement.disputed", "/settlements/16", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	tr, err := repo.DisputeSettlement(context.Background(), 16, "amount mismatch", at)
	require.NoError(t, err)
	require.True(t, tr.Applied)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSettlementRepositoryGetSettlementMapsNoRows(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newSettlementRepositoryWithSQL(nil, db)

	mock.ExpectQuery(regexp.QuoteMeta(settlementGetQuery)).
		WithArgs(int64(13)).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "merchant_id", "period_start", "period_end",
			"gross_cost", "commission", "net_payable", "status",
			"created_at", "updated_at", "confirmed_at", "paid_at", "dispute_deadline",
		}))

	got, err := repo.GetSettlement(context.Background(), 13)
	require.Nil(t, got)
	require.True(t, errors.Is(err, service.ErrSettlementNotFound))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSettlementRepositoryListExpiredPendingFiltersByDeadlineAndCursor(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newSettlementRepositoryWithSQL(nil, db)
	from := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)
	to := from.Add(7 * 24 * time.Hour)
	now := to.Add(service.SettlementDisputeWindow + time.Minute)
	deadline := to.Add(service.SettlementDisputeWindow)

	mock.ExpectQuery(regexp.QuoteMeta(settlementExpiredPendingQuery)).
		WithArgs(service.SettlementStatusPending, now, int64(5), 200).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "merchant_id", "period_start", "period_end",
			"gross_cost", "commission", "net_payable", "status",
			"created_at", "updated_at", "confirmed_at", "paid_at", "dispute_deadline",
		}).AddRow(int64(6), int64(1), from, to, 10.0, 0.8, 9.2,
			service.SettlementStatusPending, to, to, nil, nil, deadline))

	pending, err := repo.ListExpiredPendingSettlements(context.Background(), now, 5, 200)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, int64(6), pending[0].ID)
	require.NotNil(t, pending[0].DisputeDeadline)
	require.NoError(t, mock.ExpectationsWereMet())
}
