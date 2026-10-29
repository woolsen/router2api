-- 251: usage_logs 记录商户归属与写入时的抽成率快照。
-- 结算只读这里的快照，商户后续调整 commission_rate 不得回溯历史账期。
--
-- commission_rate 可空：NULL = 写入时费率未知（商户查询失败），0 = 确实为 0；
-- 平台自营行 merchant_id 与 commission_rate 都为 NULL。结算不得把 NULL 当 0。
ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS merchant_id BIGINT;
ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS commission_rate DECIMAL(10,4);
