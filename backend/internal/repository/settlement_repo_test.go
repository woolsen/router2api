package repository

import (
	"context"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
	dbent "github.com/woolsen/router2api/ent"
	"github.com/woolsen/router2api/internal/service"
)

// 结算口径是这一任务最容易出错的地方：SQL 里任何一条过滤条件丢了都会多付/少付，
// 因此这里逐条钉住 WHERE 子句与参数。
func TestSettlementRepositoryAggregateUsageFiltersSettleableRows(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newSettlementRepositoryWithSQL(nil, db)

	from := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 3, 8, 0, 0, 0, 0, time.UTC) // service 已减去 24h 缓冲

	mock.ExpectQuery(`(?s)`+
		`SUM\(actual_cost\) FILTER \(WHERE commission_rate IS NOT NULL\).*`+
		`SUM\(actual_cost \* commission_rate\) FILTER \(WHERE commission_rate IS NOT NULL\).*`+
		`COUNT\(\*\) FILTER \(WHERE commission_rate IS NULL\).*`+
		`WHERE merchant_id = \$1\s+AND created_at >= \$2\s+AND created_at < \$3\s+`+
		`AND actual_cost > 0\s+AND billing_type = \$4`,
	).
		WithArgs(int64(1), from, to, int64(service.BillingTypeBalance)).
		WillReturnRows(sqlmock.NewRows([]string{"gross", "commission", "excluded"}).
			AddRow("35.0000000000", "2.8000000000", int64(2)))

	agg, err := repo.AggregateUsage(context.Background(), 1, from, to)
	require.NoError(t, err)
	require.InDelta(t, 35, agg.GrossCost, 1e-9)
	require.InDelta(t, 2.8, agg.Commission, 1e-9)
	require.Equal(t, int64(2), agg.ExcludedUnknownCommissionRows)
	require.NoError(t, mock.ExpectationsWereMet())
}

// 补结需要从最早用量推导起始周。
func TestSettlementRepositoryEarliestSettleableUsageAt(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newSettlementRepositoryWithSQL(nil, db)
	at := time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC)

	mock.ExpectQuery(`(?s)SELECT MIN\(created_at\)\s+FROM usage_logs\s+`+
		`WHERE merchant_id = \$1\s+AND actual_cost > 0\s+AND billing_type = \$2`,
	).
		WithArgs(int64(3), int64(service.BillingTypeBalance)).
		WillReturnRows(sqlmock.NewRows([]string{"min"}).AddRow(at))

	got, err := repo.EarliestSettleableUsageAt(context.Background(), 3)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.True(t, got.Equal(at))

	// 没有任何可结算用量：MIN 返回 NULL。
	mock.ExpectQuery(`(?s)SELECT MIN\(created_at\).*`).
		WithArgs(int64(4), int64(service.BillingTypeBalance)).
		WillReturnRows(sqlmock.NewRows([]string{"min"}).AddRow(nil))

	got, err = repo.EarliestSettleableUsageAt(context.Background(), 4)
	require.NoError(t, err)
	require.Nil(t, got)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSettlementRepositoryListSettledPeriodStarts(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newSettlementRepositoryWithSQL(nil, db)
	first := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)
	second := time.Date(2026, 3, 9, 0, 0, 0, 0, time.UTC)

	mock.ExpectQuery(`(?s)SELECT period_start\s+FROM settlements\s+WHERE merchant_id = \$1\s+ORDER BY period_start`).
		WithArgs(int64(1)).
		WillReturnRows(sqlmock.NewRows([]string{"period_start"}).AddRow(first).AddRow(second))

	starts, err := repo.ListSettledPeriodStarts(context.Background(), 1)
	require.NoError(t, err)
	require.Len(t, starts, 2)
	require.True(t, starts[0].Equal(first))
	require.True(t, starts[1].Equal(second))
	require.NoError(t, mock.ExpectationsWereMet())
}

func settlementTestColumns() []string {
	return []string{
		"id", "merchant_id", "period_start", "period_end",
		"gross_cost", "commission", "net_payable", "status",
		"created_at", "updated_at", "confirmed_at", "paid_at", "dispute_deadline",
	}
}

// 新建结算单时，建单与明细写入在同一事务，明细只写一次。
func TestSettlementRepositoryCreateOrGetCreatesNewSettlementWithItems(t *testing.T) {
	db, mock := newSQLMock(t)
	drv := entsql.OpenDB(dialect.Postgres, db)
	client := dbent.NewClient(dbent.Driver(drv))
	t.Cleanup(func() { _ = client.Close() })
	repo := newSettlementRepositoryWithSQL(client, db)

	from := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 3, 9, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 3, 9, 0, 0, 1, 0, time.UTC)
	deadline := now.Add(service.SettlementDisputeWindow)

	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)INSERT INTO settlements .*ON CONFLICT \(merchant_id, period_start, period_end\) DO NOTHING\s+RETURNING id, merchant_id.*`).
		WithArgs(int64(1), from, to, 35.0, 2.8, 32.2, service.SettlementStatusPending).
		WillReturnRows(sqlmock.NewRows(settlementTestColumns()).
			AddRow(int64(9), int64(1), from, to, 35.0, 2.8, 32.2, service.SettlementStatusPending, now, now, nil, nil, deadline))
	mock.ExpectExec(`(?s)INSERT INTO settlement_items .*ON CONFLICT \(settlement_id, usage_log_id\) DO NOTHING`).
		WithArgs(int64(9), int64(1), from, to, int64(service.BillingTypeBalance)).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectCommit()

	got, err := repo.CreateOrGet(context.Background(), &service.Settlement{
		MerchantID:  1,
		PeriodStart: from,
		PeriodEnd:   to,
		GrossCost:   35,
		Commission:  2.8,
		NetPayable:  32.2,
		Status:      service.SettlementStatusPending,
	})
	require.NoError(t, err)
	require.Equal(t, int64(9), got.ID)
	require.InDelta(t, 35, got.GrossCost, 1e-9)
	require.NoError(t, mock.ExpectationsWereMet())
}

// 同一账期重跑时，INSERT ... DO NOTHING 由下一条语句回读已存在行：
// 金额不被覆盖，且不得再追加迟到的 usage 明细（否则 SUM(items.cost) > gross_cost）。
func TestSettlementRepositoryCreateOrGetConflictDoesNotAppendItems(t *testing.T) {
	db, mock := newSQLMock(t)
	drv := entsql.OpenDB(dialect.Postgres, db)
	client := dbent.NewClient(dbent.Driver(drv))
	t.Cleanup(func() { _ = client.Close() })
	repo := newSettlementRepositoryWithSQL(client, db)

	from := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 3, 9, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 3, 9, 0, 0, 1, 0, time.UTC)
	deadline := now.Add(service.SettlementDisputeWindow)

	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)INSERT INTO settlements .*ON CONFLICT \(merchant_id, period_start, period_end\) DO NOTHING\s+RETURNING id, merchant_id.*`).
		WithArgs(int64(1), from, to, 999.0, 999.0, 999.0, service.SettlementStatusPending).
		WillReturnRows(sqlmock.NewRows(settlementTestColumns()))
	mock.ExpectQuery(`(?s)SELECT id, merchant_id.*FROM settlements\s+WHERE merchant_id = \$1 AND period_start = \$2 AND period_end = \$3`).
		WithArgs(int64(1), from, to).
		WillReturnRows(sqlmock.NewRows(settlementTestColumns()).
			AddRow(int64(9), int64(1), from, to, 35.0, 2.8, 32.2, service.SettlementStatusPending, now, now, nil, nil, deadline))
	mock.ExpectCommit()

	got, err := repo.CreateOrGet(context.Background(), &service.Settlement{
		MerchantID:  1,
		PeriodStart: from,
		PeriodEnd:   to,
		GrossCost:   999, // 冲突时不应覆盖
		Commission:  999,
		NetPayable:  999,
		Status:      service.SettlementStatusPending,
	})
	require.NoError(t, err)
	require.Equal(t, int64(9), got.ID)
	require.InDelta(t, 35, got.GrossCost, 1e-9)
	require.InDelta(t, 32.2, got.NetPayable, 1e-9)
	require.NotNil(t, got.DisputeDeadline)
	require.True(t, got.DisputeDeadline.Equal(deadline))
	require.NoError(t, mock.ExpectationsWereMet())
}
