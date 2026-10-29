package repository

import (
	"context"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/woolsen/router2api/ent"
	"github.com/woolsen/router2api/internal/service"
	"github.com/stretchr/testify/require"
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

// 同一账期重跑时，ON CONFLICT DO NOTHING + 回读已存在行，金额不被覆盖。
func TestSettlementRepositoryCreateOrGetReturnsExistingOnConflict(t *testing.T) {
	db, mock := newSQLMock(t)
	drv := entsql.OpenDB(dialect.Postgres, db)
	client := dbent.NewClient(dbent.Driver(drv))
	t.Cleanup(func() { _ = client.Close() })
	repo := newSettlementRepositoryWithSQL(client, db)

	from := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 3, 9, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 3, 9, 0, 0, 1, 0, time.UTC)

	mock.ExpectQuery(`(?s)INSERT INTO "settlements".*ON CONFLICT .*DO NOTHING RETURNING "id"`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	mock.ExpectQuery(`(?s)SELECT .* FROM "settlements" WHERE .*`).
		WithArgs(int64(1), from, to).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "merchant_id", "period_start", "period_end",
			"gross_cost", "commission", "net_payable", "status",
			"created_at", "updated_at",
		}).AddRow(int64(9), int64(1), from, to, 35.0, 2.8, 32.2, service.SettlementStatusPending, now, now))

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
	require.NoError(t, mock.ExpectationsWereMet())
}
