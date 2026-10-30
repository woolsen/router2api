package repository

import (
	"context"
	"database/sql"
	"time"

	"github.com/woolsen/router2api/internal/service"
)

// merchantPayableBalanceQuery 汇总尚未打款的结算单净额。
// payable 只包含待确认/已确认（未打款）的结算单；已打款的不再是平台负债。
const merchantPayableBalanceQuery = `
SELECT COALESCE(SUM(net_payable), 0)
FROM settlements
WHERE merchant_id = $1
  AND status <> 'paid'`

// merchantDailyUsageQuery 按 UTC 自然日汇总计费用量。
// 与结算口径保持一致：只算钱包计费、actual_cost > 0 的行；
// commission_rate 为 NULL 的行费率未知，不计入抽成与应付，也不冒充 0 抽成。
const merchantDailyUsageQuery = `
SELECT
    to_char((created_at AT TIME ZONE 'UTC')::date, 'YYYY-MM-DD'),
    COUNT(*)::bigint,
    COALESCE(SUM(actual_cost) FILTER (WHERE commission_rate IS NOT NULL), 0),
    COALESCE(SUM(actual_cost * commission_rate) FILTER (WHERE commission_rate IS NOT NULL), 0),
    COALESCE(SUM(actual_cost * (1 - commission_rate)) FILTER (WHERE commission_rate IS NOT NULL), 0)
FROM usage_logs
WHERE merchant_id = $1
  AND created_at >= $2
  AND created_at < $3
  AND actual_cost > 0
  AND billing_type = $4
GROUP BY 1
ORDER BY 1`

type merchantConsoleRepository struct {
	sql sqlExecutor
}

func NewMerchantConsoleRepository(sqlDB *sql.DB) service.MerchantConsoleRepository {
	return newMerchantConsoleRepositoryWithSQL(sqlDB)
}

func newMerchantConsoleRepositoryWithSQL(sqlq sqlExecutor) *merchantConsoleRepository {
	return &merchantConsoleRepository{sql: sqlq}
}

func (r *merchantConsoleRepository) PayableBalance(ctx context.Context, merchantID int64) (float64, error) {
	var balance float64
	err := scanSingleRow(ctx, r.sql, merchantPayableBalanceQuery, []any{merchantID}, &balance)
	if err != nil {
		return 0, err
	}
	return balance, nil
}

func (r *merchantConsoleRepository) DailyUsage(
	ctx context.Context,
	merchantID int64,
	from, to time.Time,
) ([]service.MerchantDailyUsagePoint, error) {
	rows, err := r.sql.QueryContext(ctx, merchantDailyUsageQuery,
		merchantID, from, to, service.BillingTypeBalance)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []service.MerchantDailyUsagePoint
	for rows.Next() {
		var point service.MerchantDailyUsagePoint
		if err := rows.Scan(
			&point.Date,
			&point.Requests,
			&point.GrossCost,
			&point.Commission,
			&point.NetPayable,
		); err != nil {
			return nil, err
		}
		out = append(out, point)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
