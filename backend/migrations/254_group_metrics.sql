-- 254: 分组按小时聚合的请求指标。
--
-- 聚合源是 request_metrics（成功与失败都有一行）；一行代表一个
-- (group_id, hour) 唯一桶。重算使用 INSERT ... ON CONFLICT DO UPDATE 覆盖，
-- 迟到/补录的请求也能通过重跑修正，不会产生重复桶。
CREATE TABLE IF NOT EXISTS group_metric_hourly (
    id BIGSERIAL PRIMARY KEY,
    group_id BIGINT NOT NULL,
    hour TIMESTAMPTZ NOT NULL,
    requests INT NOT NULL DEFAULT 0,
    upstream_failures INT NOT NULL DEFAULT 0,
    request_errors INT NOT NULL DEFAULT 0,
    ttft_samples INT NOT NULL DEFAULT 0,
    input_tokens BIGINT NOT NULL DEFAULT 0,
    cache_read_tokens BIGINT NOT NULL DEFAULT 0,
    cache_creation_tokens BIGINT NOT NULL DEFAULT 0,
    p50_ttft DOUBLE PRECISION NOT NULL DEFAULT 0,
    p95_ttft DOUBLE PRECISION NOT NULL DEFAULT 0,
    cache_hit_rate DOUBLE PRECISION NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 唯一约束保证重算幂等；同时是 (group_id, 时间) 的读取索引。
CREATE UNIQUE INDEX IF NOT EXISTS idx_group_metric_hourly_group_hour
    ON group_metric_hourly(group_id, hour);
CREATE INDEX IF NOT EXISTS idx_group_metric_hourly_hour
    ON group_metric_hourly(hour);

-- request_metrics(created_at) 的索引在 254b_request_metrics_created_at_index_notx.sql：
-- 热表建索引必须使用非事务迁移，不能放在本事务迁移里。

COMMENT ON TABLE group_metric_hourly IS '分组按整点小时聚合的请求指标（可重算）';
COMMENT ON COLUMN group_metric_hourly.hour IS '聚合桶起点（UTC 整点）';
COMMENT ON COLUMN group_metric_hourly.requests IS '该小时请求总数（含 4xx 用户错误）';
COMMENT ON COLUMN group_metric_hourly.upstream_failures IS '归因于上游的失败数；4xx 用户错误不在此列';
COMMENT ON COLUMN group_metric_hourly.request_errors IS '4xx 用户错误数，单独展示';
COMMENT ON COLUMN group_metric_hourly.ttft_samples IS '有首包时间的流式样本数（分位数分母）';
COMMENT ON COLUMN group_metric_hourly.p50_ttft IS '流式首包 P50（毫秒）';
COMMENT ON COLUMN group_metric_hourly.p95_ttft IS '流式首包 P95（毫秒）';
COMMENT ON COLUMN group_metric_hourly.cache_hit_rate IS 'cache_read / (input + cache_read + cache_creation)';
