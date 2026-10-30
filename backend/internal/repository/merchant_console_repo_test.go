package repository

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
	"github.com/woolsen/router2api/internal/service"
)

// 商户后台的金额口径必须与结算一致；这里逐条钉住过滤条件，避免多算订阅额度
// 或把费率未知的行当成 0 抽成。
func TestMerchantConsoleRepositoryPayableBalanceExcludesPaidSettlements(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newMerchantConsoleRepositoryWithSQL(db)

	mock.ExpectQuery(`(?s)SELECT COALESCE\(SUM\(net_payable\), 0\).*` +
		`FROM settlements\s+WHERE merchant_id = \$1\s+AND status <> 'paid'`).
		WithArgs(int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"balance"}).AddRow("42.5000000000"))

	got, err := repo.PayableBalance(context.Background(), 7)
	require.NoError(t, err)
	require.InDelta(t, 42.5, got, 1e-9)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestMerchantConsoleRepositoryDailyUsageFiltersBillableRows(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newMerchantConsoleRepositoryWithSQL(db)

	from := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)

	mock.ExpectQuery(`(?s)to_char\(\(created_at AT TIME ZONE 'UTC'\)::date, 'YYYY-MM-DD'\).*`+
		`COUNT\(\*\)::bigint.*`+
		`COALESCE\(SUM\(actual_cost\) FILTER \(WHERE commission_rate IS NOT NULL\), 0\).*`+
		`FROM usage_logs\s+WHERE merchant_id = \$1\s+AND created_at >= \$2\s+`+
		`AND created_at < \$3\s+AND actual_cost > 0\s+AND billing_type = \$4\s+`+
		`GROUP BY 1\s+ORDER BY 1`).
		WithArgs(int64(7), from, to, service.BillingTypeBalance).
		WillReturnRows(sqlmock.NewRows([]string{"date", "requests", "gross", "commission", "net"}).
			AddRow("2026-10-24", int64(2), "1.0000000000", "0.0800000000", "0.9200000000").
			AddRow("2026-10-30", int64(3), "2.5000000000", "0.2000000000", "2.3000000000"))

	got, err := repo.DailyUsage(context.Background(), 7, from, to)
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, "2026-10-24", got[0].Date)
	require.Equal(t, int64(2), got[0].Requests)
	require.InDelta(t, 1.0, got[0].GrossCost, 1e-9)
	require.InDelta(t, 0.92, got[0].NetPayable, 1e-9)
	require.Equal(t, int64(3), got[1].Requests)
	// 2026-10-24 还包含一条 commission_rate IS NULL 的 5.0 用量；它必须被
	// gross 的 FILTER 排除，否则 gross != commission + net_payable。
	for _, point := range got {
		require.InDelta(t, point.GrossCost, point.Commission+point.NetPayable, 1e-9,
			"gross must equal commission + net_payable for %s", point.Date)
	}
	require.NoError(t, mock.ExpectationsWereMet())
}
