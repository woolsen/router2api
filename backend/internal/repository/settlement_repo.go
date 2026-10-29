package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/woolsen/router2api/ent"
	"github.com/woolsen/router2api/ent/merchant"
	"github.com/woolsen/router2api/ent/settlement"
	"github.com/woolsen/router2api/internal/service"
)

// settlementAggregateQuery 汇总账期内可付给商户的用量。
//
// 口径（每一句都是评审确认过的，改动前先看 Task 4 brief）：
//   - merchant_id：只用写入时快照，绝不回查商户当前配置；
//   - created_at [from, to)：to 已由 service 减去 24h 迟到写入缓冲；
//   - actual_cost > 0：排除失败请求的占位行；
//   - billing_type = $4（BillingTypeBalance=0）：只算用户实付，排除订阅额度；
//   - commission_rate IS NOT NULL：写入时费率未知的行不能按 0 结算，必须排除。
//
// 金额用 NUMERIC 汇总（actual_cost 是 decimal(20,10)），不在 SQL 里转 float。
const settlementAggregateQuery = `
SELECT
    COALESCE(SUM(actual_cost) FILTER (WHERE commission_rate IS NOT NULL), 0),
    COALESCE(SUM(actual_cost * commission_rate) FILTER (WHERE commission_rate IS NOT NULL), 0),
    COUNT(*) FILTER (WHERE commission_rate IS NULL)
FROM usage_logs
WHERE merchant_id = $1
  AND created_at >= $2
  AND created_at < $3
  AND actual_cost > 0
  AND billing_type = $4`

type settlementRepository struct {
	client *dbent.Client
	sql    sqlExecutor
}

func NewSettlementRepository(client *dbent.Client, sqlDB *sql.DB) service.SettlementRepository {
	return newSettlementRepositoryWithSQL(client, sqlDB)
}

func newSettlementRepositoryWithSQL(client *dbent.Client, sqlq sqlExecutor) *settlementRepository {
	return &settlementRepository{client: client, sql: sqlq}
}

func (r *settlementRepository) AggregateUsage(
	ctx context.Context,
	merchantID int64,
	from, to time.Time,
) (service.SettlementUsageAggregate, error) {
	var agg service.SettlementUsageAggregate
	err := scanSingleRow(ctx, r.sql, settlementAggregateQuery,
		[]any{merchantID, from, to, service.BillingTypeBalance},
		&agg.GrossCost, &agg.Commission, &agg.ExcludedUnknownCommissionRows,
	)
	if err != nil {
		return service.SettlementUsageAggregate{}, err
	}
	return agg, nil
}

func (r *settlementRepository) CreateOrGet(ctx context.Context, s *service.Settlement) (*service.Settlement, error) {
	client := clientFromContext(ctx, r.client)

	// 唯一索引兜底；并发/重跑时 ON CONFLICT DO NOTHING 不覆盖已封账金额。
	// DO NOTHING 命中冲突时没有 RETURNING 行，ent 以 sql.ErrNoRows 表达；
	// 这不是失败，随后回读已存在的那一行。
	err := client.Settlement.Create().
		SetMerchantID(s.MerchantID).
		SetPeriodStart(s.PeriodStart).
		SetPeriodEnd(s.PeriodEnd).
		SetGrossCost(s.GrossCost).
		SetCommission(s.Commission).
		SetNetPayable(s.NetPayable).
		SetStatus(s.Status).
		OnConflict(entsql.ConflictColumns(
			settlement.FieldMerchantID,
			settlement.FieldPeriodStart,
			settlement.FieldPeriodEnd,
		)).
		DoNothing().
		Exec(ctx)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	existing, err := client.Settlement.Query().
		Where(
			settlement.MerchantIDEQ(s.MerchantID),
			settlement.PeriodStartEQ(s.PeriodStart),
			settlement.PeriodEndEQ(s.PeriodEnd),
		).
		Only(ctx)
	if err != nil {
		return nil, err
	}
	return settlementEntityToService(existing), nil
}

func (r *settlementRepository) ListActiveMerchantIDs(ctx context.Context) ([]int64, error) {
	client := clientFromContext(ctx, r.client)
	return client.Merchant.Query().
		Where(
			merchant.StatusEQ(service.MerchantStatusActive),
			merchant.DeletedAtIsNil(),
		).
		IDs(ctx)
}

func settlementEntityToService(s *dbent.Settlement) *service.Settlement {
	if s == nil {
		return nil
	}
	return &service.Settlement{
		ID:          s.ID,
		MerchantID:  s.MerchantID,
		PeriodStart: s.PeriodStart,
		PeriodEnd:   s.PeriodEnd,
		GrossCost:   s.GrossCost,
		Commission:  s.Commission,
		NetPayable:  s.NetPayable,
		Status:      s.Status,
	}
}
