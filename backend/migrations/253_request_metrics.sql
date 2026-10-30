-- 253: 请求级指标（成功与失败都记录一行）。
--
-- usage_logs 只记录成功请求，分组成败率必须依赖独立的请求事实表；
-- 该表通过 request_id 与 usage_logs 关联，失败请求没有 usage_logs 也能成行。
CREATE TABLE IF NOT EXISTS request_metrics (
    id BIGSERIAL PRIMARY KEY,
    request_id VARCHAR(64) NOT NULL,
    upstream_request_id VARCHAR(128),
    user_id BIGINT NOT NULL,
    api_key_id BIGINT NOT NULL DEFAULT 0,
    group_id BIGINT NOT NULL,
    merchant_id BIGINT,
    model VARCHAR(128) NOT NULL DEFAULT '',
    status_code INT NOT NULL DEFAULT 0,
    upstream_error BOOLEAN NOT NULL DEFAULT false,
    duration_ms INT NOT NULL DEFAULT 0,
    first_token_ms INT NOT NULL DEFAULT 0,
    input_tokens INT NOT NULL DEFAULT 0,
    output_tokens INT NOT NULL DEFAULT 0,
    cache_read_tokens INT NOT NULL DEFAULT 0,
    cache_creation_tokens INT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_request_metrics_group_created ON request_metrics(group_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_request_metrics_request_id ON request_metrics(request_id);

COMMENT ON TABLE request_metrics IS '每次已鉴权网关请求的结果快照（含失败请求）';
COMMENT ON COLUMN request_metrics.status_code IS '最终写出的 HTTP 状态码；0 表示超时/未写出响应';
COMMENT ON COLUMN request_metrics.upstream_error IS '是否归因于上游：5xx 或 status_code=0；4xx 为用户错误';
COMMENT ON COLUMN request_metrics.merchant_id IS '分组商户快照；NULL 表示平台自营';
COMMENT ON COLUMN request_metrics.upstream_request_id IS '上游响应声明的请求 ID；NULL 表示未提供';
