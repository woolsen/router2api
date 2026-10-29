-- 入驻商户（中转站）：平台按 commission_rate 抽成，后续由分组/账号引用。
CREATE TABLE IF NOT EXISTS merchants (
    id BIGSERIAL PRIMARY KEY,
    name VARCHAR(100) NOT NULL,
    contact VARCHAR(200),
    status VARCHAR(20) NOT NULL DEFAULT 'active',
    commission_rate DECIMAL(10,4) NOT NULL DEFAULT 0.08,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_merchants_status ON merchants (status);
CREATE INDEX IF NOT EXISTS idx_merchants_deleted_at ON merchants (deleted_at);

COMMENT ON TABLE merchants IS '入驻商户（中转站）';
