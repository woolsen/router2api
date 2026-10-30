package repository

import (
	"context"
	"database/sql"
	"time"

	"github.com/woolsen/router2api/internal/service"
)

// merchantPayableBalanceQuery 直接读商户资金账本的可用余额。
// 唯一口径：merchant_balance.balance（已确认结算入账、提现冻结已扣除）；
// 不再从 settlements 推导，避免两套账本漂移；pending 结算从未入账。
const merchantPayableBalanceQuery = `
SELECT COALESCE((SELECT balance FROM merchant_balance WHERE merchant_id = $1), 0)`

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
