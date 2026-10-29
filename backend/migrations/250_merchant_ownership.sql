-- 商户归属：分组与账号可绑定入驻商户；NULL 表示平台自营。
-- 只做加列：索引在 250b_merchant_ownership_index_notx.sql 里并发创建，
-- 避免在 groups/accounts 热表上持 SHARE 锁阻塞写入。
ALTER TABLE groups   ADD COLUMN IF NOT EXISTS merchant_id BIGINT;
ALTER TABLE accounts ADD COLUMN IF NOT EXISTS merchant_id BIGINT;
