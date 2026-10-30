-- 申诉与调账。
-- 发起方：商户针对自己的结算单，用户针对自己的单条 usage_logs。
-- 资金动作只通过 merchant_ledger 记录，且 adjust 必须引用 dispute。

CREATE TABLE IF NOT EXISTS disputes (
    id            BIGSERIAL PRIMARY KEY,
    owner_type    VARCHAR(20) NOT NULL,
    owner_id      BIGINT NOT NULL,
    target_type   VARCHAR(20) NOT NULL,
    target_id     BIGINT NOT NULL,
    amount        DECIMAL(20,10) NOT NULL DEFAULT 0,
    reason        TEXT NOT NULL,
    status        VARCHAR(20) NOT NULL DEFAULT 'open',
    decision      VARCHAR(20),
    adjust_amount DECIMAL(20,10) NOT NULL DEFAULT 0,
    note          TEXT,
    resolved_by   VARCHAR(100),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    resolved_at   TIMESTAMPTZ
);

-- 同一目标同时只能有一条 open 申诉；裁决后允许在窗口内重新发起。
CREATE UNIQUE INDEX IF NOT EXISTS idx_disputes_open_target
    ON disputes(target_type, target_id) WHERE status = 'open';

CREATE INDEX IF NOT EXISTS idx_disputes_owner_created
    ON disputes(owner_type, owner_id, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_disputes_status_created
    ON disputes(status, created_at DESC);

COMMENT ON TABLE disputes IS '申诉单；商户申诉结算单，用户申诉单条用量';
COMMENT ON COLUMN disputes.owner_type IS 'merchant 商户 / user 普通用户';
COMMENT ON COLUMN disputes.owner_id IS '发起方 ID：merchants.id 或 users.id';
COMMENT ON COLUMN disputes.target_type IS 'settlement 结算单 / usage 单条 usage_logs';
COMMENT ON COLUMN disputes.amount IS '商户/用户主张的差异金额';
COMMENT ON COLUMN disputes.status IS 'open 待裁决 / resolved 已裁决';
COMMENT ON COLUMN disputes.decision IS 'reject 驳回 / confirm 认可 / adjust 调账';
COMMENT ON COLUMN disputes.adjust_amount IS '调账增量：负数扣减，正数补足';
COMMENT ON COLUMN disputes.note IS '裁决说明';
COMMENT ON COLUMN disputes.resolved_by IS '裁决操作人';
