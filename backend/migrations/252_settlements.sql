-- 商户周结算单（T+7）。金额沿用 usage_logs.actual_cost 的 DECIMAL(20,10) 口径，
-- 周期时间统一存 UTC，周一 00:00 为账期边界。
CREATE TABLE IF NOT EXISTS settlements (
    id BIGSERIAL PRIMARY KEY,
    merchant_id BIGINT NOT NULL,
    period_start TIMESTAMPTZ NOT NULL,
    period_end   TIMESTAMPTZ NOT NULL,
    gross_cost   DECIMAL(20,10) NOT NULL DEFAULT 0,
    commission   DECIMAL(20,10) NOT NULL DEFAULT 0,
    net_payable  DECIMAL(20,10) NOT NULL DEFAULT 0,
    status       VARCHAR(20) NOT NULL DEFAULT 'pending',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 生成幂等：同一商户同一账期只允许一张结算单。
CREATE UNIQUE INDEX IF NOT EXISTS idx_settlements_merchant_period
    ON settlements(merchant_id, period_start, period_end);
CREATE INDEX IF NOT EXISTS idx_settlements_status ON settlements (status);

COMMENT ON TABLE settlements IS '商户周结算单（应付，T+7）';
COMMENT ON COLUMN settlements.gross_cost IS '账期内用户实付合计';
COMMENT ON COLUMN settlements.commission IS '平台抽成合计';
COMMENT ON COLUMN settlements.net_payable IS '应付商户金额 = gross_cost - commission';
COMMENT ON COLUMN settlements.status IS 'pending 待确认 / confirmed 已确认 / paid 已打款';
