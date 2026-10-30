-- 商户提现单。
-- 资金账本（merchant_balance / merchant_ledger）由 256 建立；
-- 本迁移只新增 withdrawals，不重复建账本表。
-- 金额精度与账本一致，使用 DECIMAL(20,10)。

CREATE TABLE IF NOT EXISTS withdrawals (
    id            BIGSERIAL PRIMARY KEY,
    merchant_id   BIGINT NOT NULL,
    amount        DECIMAL(20,10) NOT NULL,
    status        VARCHAR(20) NOT NULL DEFAULT 'requested',
    proof_path    VARCHAR(500),
    proof_sha256  CHAR(64),
    approved_by   VARCHAR(100),
    requested_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    paid_at       TIMESTAMPTZ
);

-- 同一商户同时只能有一笔在途提现（待审核或待打款）。
-- 这是并发提现的最后一道防线；Request 事务内仍会先锁 merchant_balance 行。
CREATE UNIQUE INDEX IF NOT EXISTS idx_withdrawals_open
    ON withdrawals(merchant_id) WHERE status IN ('requested','approved');

CREATE INDEX IF NOT EXISTS idx_withdrawals_merchant_requested
    ON withdrawals(merchant_id, requested_at DESC);

CREATE INDEX IF NOT EXISTS idx_withdrawals_status
    ON withdrawals(status);

COMMENT ON TABLE withdrawals IS '商户提现单；申请时余额转入冻结，驳回解冻，打款释放冻结';
COMMENT ON COLUMN withdrawals.status IS 'requested 待审核 / approved 待打款 / paid 已打款 / rejected 已驳回';
COMMENT ON COLUMN withdrawals.proof_path IS '运营打款凭证的存储路径';
COMMENT ON COLUMN withdrawals.proof_sha256 IS '打款凭证 SHA-256，用于防篡改核对';
COMMENT ON COLUMN withdrawals.approved_by IS '审核或打款操作人（邮箱/标识）';
