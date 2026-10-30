-- 结算单运营状态机、用量明细与商户资金账本。
-- 资金表是 Task 5 提现可用额的唯一读取来源：确认结算单时同事务入账。

ALTER TABLE settlements ADD COLUMN IF NOT EXISTS confirmed_at TIMESTAMPTZ;
ALTER TABLE settlements ADD COLUMN IF NOT EXISTS paid_at TIMESTAMPTZ;
ALTER TABLE settlements ADD COLUMN IF NOT EXISTS dispute_deadline TIMESTAMPTZ;

-- M1 已生成的结算单按 created_at + 7 天补齐申诉截止时间。
UPDATE settlements
SET dispute_deadline = created_at + INTERVAL '7 days'
WHERE dispute_deadline IS NULL;

ALTER TABLE settlements ALTER COLUMN dispute_deadline SET DEFAULT (NOW() + INTERVAL '7 days');
ALTER TABLE settlements ALTER COLUMN dispute_deadline SET NOT NULL;

CREATE INDEX IF NOT EXISTS idx_settlements_pending_deadline
    ON settlements (dispute_deadline)
    WHERE status = 'pending';

CREATE TABLE IF NOT EXISTS settlement_items (
    id            BIGSERIAL PRIMARY KEY,
    settlement_id BIGINT NOT NULL,
    usage_log_id  BIGINT NOT NULL,
    cost          DECIMAL(20,10) NOT NULL,
    commission    DECIMAL(20,10) NOT NULL
);

-- 同一结算单同一用量只允许一条明细；Generate 重跑不会重复写入。
CREATE UNIQUE INDEX IF NOT EXISTS idx_settlement_items_unique
    ON settlement_items(settlement_id, usage_log_id);

CREATE TABLE IF NOT EXISTS merchant_balance (
    merchant_id BIGINT PRIMARY KEY,
    balance     DECIMAL(20,10) NOT NULL DEFAULT 0,
    frozen      DECIMAL(20,10) NOT NULL DEFAULT 0,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS merchant_ledger (
    id         BIGSERIAL PRIMARY KEY,
    merchant_id BIGINT NOT NULL,
    type       VARCHAR(32) NOT NULL,
    amount     DECIMAL(20,10) NOT NULL,
    ref_type   VARCHAR(32) NOT NULL,
    ref_id     BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 资金流水幂等键：同一业务单据（type + ref_type + ref_id）只入账一次。
CREATE UNIQUE INDEX IF NOT EXISTS idx_merchant_ledger_source
    ON merchant_ledger(type, ref_type, ref_id);
CREATE INDEX IF NOT EXISTS idx_merchant_ledger_merchant_created
    ON merchant_ledger(merchant_id, created_at DESC);

-- 为 settlement_items 上线前已生成的结算单回填明细。
-- 过滤条件必须与 settlementAggregateQuery 保持一致：
--   actual_cost > 0（排除失败请求占位行）
--   billing_type = 0（BillingTypeBalance，只算用户实付）
--   commission_rate IS NOT NULL（费率未知的行不按 0 结算）
-- ON CONFLICT 保证重复执行不会追加已于建单时写入的明细。
INSERT INTO settlement_items (settlement_id, usage_log_id, cost, commission)
SELECT s.id, u.id, u.actual_cost, u.actual_cost * u.commission_rate
FROM settlements s
JOIN usage_logs u
  ON u.merchant_id = s.merchant_id
 AND u.created_at >= s.period_start
 AND u.created_at < s.period_end
WHERE u.actual_cost > 0
  AND u.billing_type = 0
  AND u.commission_rate IS NOT NULL
ON CONFLICT (settlement_id, usage_log_id) DO NOTHING;

COMMENT ON COLUMN settlements.status IS 'pending 待确认 / confirmed 已确认 / disputed 申诉中 / paid 已打款';
COMMENT ON COLUMN settlements.confirmed_at IS '确认时间（手动或 7 天自动确认）';
COMMENT ON COLUMN settlements.paid_at IS '打款完成时间';
COMMENT ON COLUMN settlements.dispute_deadline IS '申诉截止时间 = created_at + 7 days';
COMMENT ON TABLE settlement_items IS '结算单用量明细，生成结算单时逐条快照';
COMMENT ON TABLE merchant_balance IS '商户资金余额（唯一可加锁账本行）；balance 是可用额（提现口径），frozen 是冻结额（申诉等）';
COMMENT ON COLUMN merchant_balance.balance IS '可用余额；Task 5 的提现可用额直接读此列（Available = balance）';
COMMENT ON COLUMN merchant_balance.frozen IS '冻结余额；已确认结算单申诉时从 balance 转入';
COMMENT ON TABLE merchant_ledger IS '商户资金流水；UNIQUE(type, ref_type, ref_id) 保证同源单据只入账一次';
