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
		WillReturnRows(sqlmock.NewRows([]string{"status", "dispute_deadline"}).
			AddRow(service.SettlementStatusPending, deadline))
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
		WillReturnRows(sqlmock.NewRows([]string{"status", "dispute_deadline"}).
			AddRow(service.SettlementStatusPending, deadline))
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
