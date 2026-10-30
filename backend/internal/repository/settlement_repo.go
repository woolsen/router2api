package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	dbent "github.com/woolsen/router2api/ent"
	"github.com/woolsen/router2api/ent/merchant"
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

// settlementEarliestUsageQuery 找出该商户最早一条可结算用量，用于从历史起点逐周补结。
// 不按 commission_rate 过滤：即使某周只有费率未知的行，也要生成（零额）结算单，
// 让 Generate 里的跳过日志留下可核对的记录。
const settlementEarliestUsageQuery = `
SELECT MIN(created_at)
FROM usage_logs
WHERE merchant_id = $1
  AND actual_cost > 0
  AND billing_type = $2`

const settlementSettledPeriodStartsQuery = `
SELECT period_start
FROM settlements
WHERE merchant_id = $1
ORDER BY period_start`

// settlementColumns 是 service.Settlement 的完整读取列，顺序必须与 settlementScanDest 一致。
const settlementColumns = `id, merchant_id, period_start, period_end, gross_cost, commission,
net_payable, status, created_at, updated_at, confirmed_at, paid_at, dispute_deadline`

// settlementItemsInsertQuery 把账期内可结算用量逐条写入明细。
// 过滤条件必须与 settlementAggregateQuery 保持一致，否则明细与汇总金额会漂移。
const settlementItemsInsertQuery = `
INSERT INTO settlement_items (settlement_id, usage_log_id, cost, commission)
SELECT $1, id, actual_cost, actual_cost * commission_rate
FROM usage_logs
WHERE merchant_id = $2
  AND created_at >= $3
  AND created_at < $4
  AND actual_cost > 0
  AND billing_type = $5
  AND commission_rate IS NOT NULL
ON CONFLICT (settlement_id, usage_log_id) DO NOTHING`

// settlementInsertQuery 幂等建单：唯一索引冲突时 DO NOTHING，由下一条 SELECT 回读。
// created_at / dispute_deadline 使用数据库默认值，保证 deadline 严格等于 created_at + 7 days。
//
// 不能用 CTE 在一条语句里同时 INSERT ... ON CONFLICT DO NOTHING 和 SELECT：
// PostgreSQL 的语句快照在语句开始时取得。并发事务尚未提交时，INSERT 会等对方提交，
// 但同一语句里的 SELECT 仍用旧快照，看不到刚提交的冲突行，整条语句返回 0 行。
const settlementInsertQuery = `
INSERT INTO settlements (merchant_id, period_start, period_end, gross_cost, commission, net_payable, status)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (merchant_id, period_start, period_end) DO NOTHING
RETURNING ` + settlementColumns

// settlementSelectByPeriodQuery 必须作为 INSERT 之后的独立语句执行：
// 新语句会重新取快照，能看到并发事务刚刚提交的冲突行。
const settlementSelectByPeriodQuery = `
SELECT ` + settlementColumns + `
FROM settlements
WHERE merchant_id = $1 AND period_start = $2 AND period_end = $3`

type sqlBeginner interface {
	BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error)
}

type settlementRepository struct {
	client *dbent.Client
	sql    sqlExecutor
	db     sqlBeginner
}

func NewSettlementRepository(client *dbent.Client, sqlDB *sql.DB) service.SettlementRepository {
	return newSettlementRepositoryWithSQL(client, sqlDB)
}

func newSettlementRepositoryWithSQL(client *dbent.Client, sqlq sqlExecutor) *settlementRepository {
	repo := &settlementRepository{client: client, sql: sqlq}
	if beginner, ok := sqlq.(sqlBeginner); ok {
		repo.db = beginner
	}
	return repo
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
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("settlement repository is not transactional")
	}
	if s == nil {
		return nil, fmt.Errorf("nil settlement")
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var out service.Settlement
	var createdAt, updatedAt time.Time
	dest := []any{
		&out.ID,
		&out.MerchantID,
		&out.PeriodStart,
		&out.PeriodEnd,
		&out.GrossCost,
		&out.Commission,
		&out.NetPayable,
		&out.Status,
		&createdAt,
		&updatedAt,
		&out.ConfirmedAt,
		&out.PaidAt,
		&out.DisputeDeadline,
	}

	created := true
	err = scanSingleRow(ctx, tx, settlementInsertQuery,
		[]any{
			s.MerchantID,
			s.PeriodStart.UTC(),
			s.PeriodEnd.UTC(),
			s.GrossCost,
			s.Commission,
			s.NetPayable,
			s.Status,
		}, dest...)
	if errors.Is(err, sql.ErrNoRows) {
		// 唯一键冲突：同一事务里的新语句能看到对方已提交的行。
		created = false
		err = scanSingleRow(ctx, tx, settlementSelectByPeriodQuery,
			[]any{s.MerchantID, s.PeriodStart.UTC(), s.PeriodEnd.UTC()}, dest...)
	}
	if err != nil {
		return nil, err
	}

	// 明细只在本次真正新建结算单时写入。重跑已存在的结算单不得追加迟到的 usage 行，
	// 否则 SUM(items.cost) 会超过已封账的 gross_cost。
	if created {
		if _, err := tx.ExecContext(ctx, settlementItemsInsertQuery,
			out.ID, out.MerchantID, out.PeriodStart.UTC(), out.PeriodEnd.UTC(), service.BillingTypeBalance,
		); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &out, nil
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

func (r *settlementRepository) EarliestSettleableUsageAt(ctx context.Context, merchantID int64) (*time.Time, error) {
	var earliest *time.Time
	err := scanSingleRow(ctx, r.sql, settlementEarliestUsageQuery,
		[]any{merchantID, service.BillingTypeBalance},
		&earliest,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// MIN() 聚合必然返回一行，这里只是防御性处理。
			return nil, nil
		}
		return nil, err
	}
	return earliest, nil
}

func (r *settlementRepository) ListSettledPeriodStarts(ctx context.Context, merchantID int64) ([]time.Time, error) {
	rows, err := r.sql.QueryContext(ctx, settlementSettledPeriodStartsQuery, merchantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var starts []time.Time
	for rows.Next() {
		var start time.Time
		if err := rows.Scan(&start); err != nil {
			return nil, err
		}
		starts = append(starts, start.UTC())
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return starts, nil
}
