-- usage_logs 是写入热表，索引用 CONCURRENTLY 单独建，避免加列迁移里长时间阻塞写入。
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_usage_logs_merchant_created
    ON usage_logs(merchant_id, created_at) WHERE merchant_id IS NOT NULL;
