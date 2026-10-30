package repository

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"

	"github.com/woolsen/router2api/internal/service"
)

func disputeTestAt() time.Time {
	return time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
}

func disputeTestRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "owner_type", "owner_id", "target_type", "target_id", "amount",
		"reason", "status", "decision", "adjust_amount", "note", "resolved_by",
		"created_at", "resolved_at",
	})
}

func addDisputeTestRow(
	rows *sqlmock.Rows,
	id int64,
	ownerType string,
	ownerID int64,
	targetType string,
	targetID int64,
	amount float64,
	status string,
	at time.Time,
) *sqlmock.Rows {
	return rows.AddRow(
		id, ownerType, ownerID, targetType, targetID, amount,
		"amount mismatch", status, "", 0.0, "", "", at, nil,
	)
}

func expectOpenSettlementLock(
	mock sqlmock.Sqlmock,
	settlementID, merchantID int64,
	status string,
	netPayable float64,
	deadline time.Time,
) {
	mock.ExpectQuery(regexp.QuoteMeta(settlementForDisputeLockQuery)).
		WithArgs(settlementID).
		WillReturnRows(sqlmock.NewRows([]string{"status", "merchant_id", "net_payable", "dispute_deadline"}).
			AddRow(status, merchantID, netPayable, deadline))
}

// 来源 B：confirmed → disputed 必须在同一事务内把净额从 balance 转入 frozen。
func TestDisputeOpenFreezesConfirmedBalance(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newDisputeRepositoryWithSQL(db)
	at := disputeTestAt()
	deadline := at.Add(48 * time.Hour)

	mock.ExpectBegin()
	expectOpenSettlementLock(mock, 9, 7, service.SettlementStatusConfirmed, 80.0, deadline)
	mock.ExpectQuery(regexp.QuoteMeta(disputeInsertQuery)).
		WithArgs("merchant", int64(7), service.DisputeTargetSettlement, int64(9), 3.0, "用量偏高", at).
		WillReturnRows(addDisputeTestRow(disputeTestRows(), 1, "merchant", 7,
			service.DisputeTargetSettlement, 9, 3, service.DisputeStatusOpen, at))
	mock.ExpectExec(regexp.QuoteMeta(settlementToDisputedUpdate)).
		WithArgs(int64(9), at, service.SettlementStatusConfirmed).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(disputeHoldLedgerInsert)).
		WithArgs(int64(9), int64(1), at).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(settlementDisputeHoldBalance)).
		WithArgs(int64(9), at).
		WillReturnRows(sqlmock.NewRows([]string{"balance"}).AddRow(19.5))
	mock.ExpectExec(regexp.QuoteMeta(disputeAuditInsert)).
		WithArgs(at, "merchant", "dispute.opened", "/disputes/1", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	d, err := repo.OpenSettlement(context.Background(), service.OpenDisputeInput{
		OwnerType: service.DisputeOwnerMerchant, OwnerID: 7,
		TargetType: service.DisputeTargetSettlement, TargetID: 9,
		Amount: 3, Reason: "用量偏高",
	}, at)
	require.NoError(t, err)
	require.Equal(t, service.DisputeStatusOpen, d.Status)
	require.NoError(t, mock.ExpectationsWereMet())
}

// 来源 A：pending 从未入账，申诉时不得冻结。
func TestDisputeOpenPendingDoesNotFreeze(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newDisputeRepositoryWithSQL(db)
	at := disputeTestAt()

	mock.ExpectBegin()
	expectOpenSettlementLock(mock, 10, 7, service.SettlementStatusPending, 42.0, at.Add(time.Hour))
	mock.ExpectQuery(regexp.QuoteMeta(disputeInsertQuery)).
		WithArgs("merchant", int64(7), service.DisputeTargetSettlement, int64(10), 1.0, "amount mismatch", at).
		WillReturnRows(addDisputeTestRow(disputeTestRows(), 2, "merchant", 7,
			service.DisputeTargetSettlement, 10, 1, service.DisputeStatusOpen, at))
	mock.ExpectExec(regexp.QuoteMeta(settlementToDisputedUpdate)).
		WithArgs(int64(10), at, service.SettlementStatusPending).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(disputeAuditInsert)).
		WithArgs(at, "merchant", "dispute.opened", "/disputes/2", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	_, err := repo.OpenSettlement(context.Background(), service.OpenDisputeInput{
		OwnerType: service.DisputeOwnerMerchant, OwnerID: 7,
		TargetType: service.DisputeTargetSettlement, TargetID: 10,
		Amount: 1, Reason: "amount mismatch",
	}, at)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestDisputeOpenWindowClosed(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newDisputeRepositoryWithSQL(db)
	at := disputeTestAt()

	mock.ExpectBegin()
	expectOpenSettlementLock(mock, 11, 7, service.SettlementStatusConfirmed, 80, at.Add(-time.Minute))
	mock.ExpectRollback()

	_, err := repo.OpenSettlement(context.Background(), service.OpenDisputeInput{
		OwnerType: service.DisputeOwnerMerchant, OwnerID: 7,
		TargetID: 11, Amount: 1, Reason: "late",
	}, at)
	require.ErrorIs(t, err, service.ErrDisputeWindowClosed)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestDisputeOpenPaidSettlementRejected(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newDisputeRepositoryWithSQL(db)
	at := disputeTestAt()

	mock.ExpectBegin()
	expectOpenSettlementLock(mock, 12, 7, service.SettlementStatusPaid, 80, at.Add(time.Hour))
	mock.ExpectRollback()

	_, err := repo.OpenSettlement(context.Background(), service.OpenDisputeInput{
		OwnerType: service.DisputeOwnerMerchant, OwnerID: 7,
		TargetID: 12, Amount: 1, Reason: "already paid",
	}, at)
	require.ErrorIs(t, err, service.ErrDisputeSettlementPaid)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestDisputeOpenForeignSettlementIsNotFound(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newDisputeRepositoryWithSQL(db)
	at := disputeTestAt()

	mock.ExpectBegin()
	expectOpenSettlementLock(mock, 13, 8, service.SettlementStatusConfirmed, 80, at.Add(time.Hour))
	mock.ExpectRollback()

	_, err := repo.OpenSettlement(context.Background(), service.OpenDisputeInput{
		OwnerType: service.DisputeOwnerMerchant, OwnerID: 7,
		TargetID: 13, Amount: 1, Reason: "not mine",
	}, at)
	require.ErrorIs(t, err, service.ErrDisputeTargetNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestDisputeOpenDuplicateIsRejected(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newDisputeRepositoryWithSQL(db)
	at := disputeTestAt()

	mock.ExpectBegin()
	expectOpenSettlementLock(mock, 14, 7, service.SettlementStatusPending, 80, at.Add(time.Hour))
	mock.ExpectQuery(regexp.QuoteMeta(disputeInsertQuery)).
		WithArgs("merchant", int64(7), service.DisputeTargetSettlement, int64(14), 1.0, "dup", at).
		WillReturnError(&pq.Error{Code: "23505"})
	mock.ExpectRollback()

	_, err := repo.OpenSettlement(context.Background(), service.OpenDisputeInput{
		OwnerType: service.DisputeOwnerMerchant, OwnerID: 7,
		TargetID: 14, Amount: 1, Reason: "dup",
	}, at)
	require.ErrorIs(t, err, service.ErrDisputeAlreadyOpen)
	require.NoError(t, mock.ExpectationsWereMet())
}

func expectResolveFixture(
	mock sqlmock.Sqlmock,
	disputeID, targetID, ownerID int64,
	targetType string,
	netPayable float64,
	hasSettle bool,
	at time.Time,
) {
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(disputeLockQuery)).
		WithArgs(disputeID).
		WillReturnRows(addDisputeTestRow(disputeTestRows(), disputeID,
			service.DisputeOwnerMerchant, ownerID, targetType, targetID, 3,
			service.DisputeStatusOpen, at))
	if targetType == service.DisputeTargetSettlement {
		mock.ExpectQuery(regexp.QuoteMeta(settlementForDisputeLockQuery)).
			WithArgs(targetID).
			WillReturnRows(sqlmock.NewRows([]string{"status", "merchant_id", "net_payable", "dispute_deadline"}).
				AddRow(service.SettlementStatusDisputed, ownerID, netPayable, at.Add(time.Hour)))
		mock.ExpectQuery(regexp.QuoteMeta(settlementHasSettleLedgerQuery)).
			WithArgs(targetID).
			WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(hasSettle))
	}
}

// 来源 B 驳回：只释放冻结，绝不重复写 settle。
func TestDisputeResolveRejectReleasesFreeze(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newDisputeRepositoryWithSQL(db)
	at := disputeTestAt()

	expectResolveFixture(mock, 1, 9, 7, service.DisputeTargetSettlement, 80, true, at)
	mock.ExpectExec(regexp.QuoteMeta(settlementResolveConfirmedUpdate)).
		WithArgs(int64(9), at).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(disputeReleaseLedgerInsert)).
		WithArgs(int64(7), 80.0, int64(1), at).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(disputeReleaseBalanceQuery)).
		WithArgs(int64(7), 80.0, at).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(disputeResolveUpdate)).
		WithArgs(int64(1), service.DisputeDecisionReject, 0.0, "", "admin@example.com", at).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(disputeAuditInsert)).
		WithArgs(at, "admin@example.com", "dispute.resolved", "/disputes/1", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	d, err := repo.Resolve(context.Background(), 1, service.ResolveInput{
		Decision: service.DisputeDecisionReject, By: "admin@example.com",
	}, at)
	require.NoError(t, err)
	require.Equal(t, service.DisputeStatusResolved, d.Status)
	require.NoError(t, mock.ExpectationsWereMet())
}

// 来源 A 驳回：回到 pending 并重新给 7 天窗口，不动钱、不写 settle。
func TestDisputeResolveRejectSourceAReturnsPending(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newDisputeRepositoryWithSQL(db)
	at := disputeTestAt()

	expectResolveFixture(mock, 2, 10, 7, service.DisputeTargetSettlement, 40, false, at)
	mock.ExpectExec(regexp.QuoteMeta(settlementResolvePendingUpdate)).
		WithArgs(int64(10), at.Add(service.SettlementDisputeWindow), at).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(disputeResolveUpdate)).
		WithArgs(int64(2), service.DisputeDecisionReject, 0.0, "", "admin", at).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(disputeAuditInsert)).
		WithArgs(at, "admin", "dispute.resolved", "/disputes/2", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	_, err := repo.Resolve(context.Background(), 2, service.ResolveInput{
		Decision: service.DisputeDecisionReject, By: "admin",
	}, at)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

// 来源 A 确认：必须补写唯一 settle 并给余额入账一次。
func TestDisputeResolveConfirmSourceACreditsOnce(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newDisputeRepositoryWithSQL(db)
	at := disputeTestAt()

	expectResolveFixture(mock, 3, 11, 7, service.DisputeTargetSettlement, 80, false, at)
	mock.ExpectExec(regexp.QuoteMeta(settlementResolveConfirmedUpdate)).
		WithArgs(int64(11), at).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(settlementLedgerInsert)).
		WithArgs(int64(11), at).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(settlementBalanceCredit)).
		WithArgs(int64(11), at).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(disputeResolveUpdate)).
		WithArgs(int64(3), service.DisputeDecisionConfirm, 0.0, "", "admin", at).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(disputeAuditInsert)).
		WithArgs(at, "admin", "dispute.resolved", "/disputes/3", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	_, err := repo.Resolve(context.Background(), 3, service.ResolveInput{
		Decision: service.DisputeDecisionConfirm, By: "admin",
	}, at)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

// 来源 B 确认：只释放冻结；任何额外的 settle 插入都会被 sqlmock 判定为未预期调用。
func TestDisputeResolveConfirmSourceBDoesNotDoubleCredit(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newDisputeRepositoryWithSQL(db)
	at := disputeTestAt()

	expectResolveFixture(mock, 4, 12, 7, service.DisputeTargetSettlement, 80, true, at)
	mock.ExpectExec(regexp.QuoteMeta(settlementResolveConfirmedUpdate)).
		WithArgs(int64(12), at).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(disputeReleaseLedgerInsert)).
		WithArgs(int64(7), 80.0, int64(4), at).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(disputeReleaseBalanceQuery)).
		WithArgs(int64(7), 80.0, at).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(disputeResolveUpdate)).
		WithArgs(int64(4), service.DisputeDecisionConfirm, 0.0, "", "admin", at).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(disputeAuditInsert)).
		WithArgs(at, "admin", "dispute.resolved", "/disputes/4", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	_, err := repo.Resolve(context.Background(), 4, service.ResolveInput{
		Decision: service.DisputeDecisionConfirm, By: "admin",
	}, at)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

// 来源 B 调账：先从冻结额扣 3，再释放剩余 77；adjust 流水挂 dispute。
func TestDisputeResolveAdjustWritesLedgerWithDisputeRef(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newDisputeRepositoryWithSQL(db)
	at := disputeTestAt()

	expectResolveFixture(mock, 5, 13, 7, service.DisputeTargetSettlement, 80, true, at)
	mock.ExpectExec(regexp.QuoteMeta(settlementResolveConfirmedUpdate)).
		WithArgs(int64(13), at).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(disputeReleaseLedgerInsert)).
		WithArgs(int64(7), 80.0, int64(5), at).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(disputeAdjustDeductFrozenQuery)).
		WithArgs(int64(7), 3.0, at).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(disputeAdjustReleaseQuery)).
		WithArgs(int64(7), 77.0, 77.0, at).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(disputeAdjustLedgerInsert)).
		WithArgs(int64(7), -3.0, int64(5), at).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(disputeResolveUpdate)).
		WithArgs(int64(5), service.DisputeDecisionAdjust, -3.0, "核对误差", "admin", at).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(disputeAuditInsert)).
		WithArgs(at, "admin", "dispute.resolved", "/disputes/5", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	_, err := repo.Resolve(context.Background(), 5, service.ResolveInput{
		Decision: service.DisputeDecisionAdjust, AdjustAmount: -3,
		Note: "核对误差", By: "admin",
	}, at)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

// 来源 A 调账：先补 settle 入账，再把负向调账记到 dispute 流水。
func TestDisputeResolveAdjustSourceACreditsThenAdjusts(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newDisputeRepositoryWithSQL(db)
	at := disputeTestAt()

	expectResolveFixture(mock, 6, 14, 7, service.DisputeTargetSettlement, 80, false, at)
	mock.ExpectExec(regexp.QuoteMeta(settlementResolveConfirmedUpdate)).
		WithArgs(int64(14), at).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(settlementLedgerInsert)).
		WithArgs(int64(14), at).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(settlementBalanceCredit)).
		WithArgs(int64(14), at).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(disputeAdjustLedgerInsert)).
		WithArgs(int64(7), -3.0, int64(6), at).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(disputeAdjustCreditQuery)).
		WithArgs(int64(7), -3.0, at).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(disputeResolveUpdate)).
		WithArgs(int64(6), service.DisputeDecisionAdjust, -3.0, "", "admin", at).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(disputeAuditInsert)).
		WithArgs(at, "admin", "dispute.resolved", "/disputes/6", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	_, err := repo.Resolve(context.Background(), 6, service.ResolveInput{
		Decision: service.DisputeDecisionAdjust, AdjustAmount: -3, By: "admin",
	}, at)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestDisputeAdjustLedgerRejectsUnattributed(t *testing.T) {
	repo := newDisputeRepositoryWithSQL(nil)
	err := repo.AdjustLedger(context.Background(), 7, -3, "", 0, disputeTestAt())
	require.ErrorIs(t, err, service.ErrAdjustRequiresSource)
}

func TestDisputeAdjustLedgerWritesDisputeReference(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newDisputeRepositoryWithSQL(db)
	at := disputeTestAt()

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(disputeOwnerQuery)).
		WithArgs(int64(5)).
		WillReturnRows(sqlmock.NewRows([]string{"owner_type", "owner_id"}).
			AddRow(service.DisputeOwnerMerchant, int64(7)))
	mock.ExpectExec(regexp.QuoteMeta(disputeAdjustLedgerInsert)).
		WithArgs(int64(7), -3.0, int64(5), at).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(disputeAdjustCreditQuery)).
		WithArgs(int64(7), -3.0, at).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	require.NoError(t, repo.AdjustLedger(context.Background(), 7, -3, "dispute", 5, at))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestDisputeAdjustLedgerRejectsForeignDispute(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newDisputeRepositoryWithSQL(db)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(disputeOwnerQuery)).
		WithArgs(int64(6)).
		WillReturnRows(sqlmock.NewRows([]string{"owner_type", "owner_id"}).
			AddRow(service.DisputeOwnerMerchant, int64(8)))
	mock.ExpectRollback()

	err := repo.AdjustLedger(context.Background(), 7, -3, "dispute", 6, disputeTestAt())
	require.ErrorIs(t, err, service.ErrDisputeNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestDisputeOpenUsageRequiresOwnership(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newDisputeRepositoryWithSQL(db)
	at := disputeTestAt()

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(usageLogOwnerQuery)).
		WithArgs(int64(99)).
		WillReturnRows(sqlmock.NewRows([]string{"user_id"}).AddRow(int64(42)))
	mock.ExpectRollback()

	_, err := repo.OpenUsage(context.Background(), service.OpenDisputeInput{
		OwnerType: service.DisputeOwnerUser, OwnerID: 7,
		TargetType: service.DisputeTargetUsage, TargetID: 99,
		Amount: 1, Reason: "wrong charge",
	}, at)
	require.ErrorIs(t, err, service.ErrDisputeTargetNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestDisputeResolveUsageAdjustCreditsUser(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newDisputeRepositoryWithSQL(db)
	at := disputeTestAt()

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(disputeLockQuery)).
		WithArgs(int64(7)).
		WillReturnRows(addDisputeTestRow(disputeTestRows(), 7,
			service.DisputeOwnerUser, 42, service.DisputeTargetUsage, 99, 5,
			service.DisputeStatusOpen, at))
	mock.ExpectExec(regexp.QuoteMeta(disputeUserBalanceQuery)).
		WithArgs(5.0, at, int64(42)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(disputeResolveUpdate)).
		WithArgs(int64(7), service.DisputeDecisionAdjust, 5.0, "double charged", "admin", at).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(disputeAuditInsert)).
		WithArgs(at, "admin", "dispute.resolved", "/disputes/7", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	_, err := repo.Resolve(context.Background(), 7, service.ResolveInput{
		Decision: service.DisputeDecisionAdjust, AdjustAmount: 5,
		Note: "double charged", By: "admin",
	}, at)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}
