-- 251: usage_logs 记录商户归属与写入时的抽成率快照。
-- 结算只读这里的快照，商户后续调整 commission_rate 不得回溯历史账期。
ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS merchant_id BIGINT;
ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS commission_rate DECIMAL(10,4) NOT NULL DEFAULT 0;
