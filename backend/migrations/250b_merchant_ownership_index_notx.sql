-- 250b_merchant_ownership_index_notx.sql
-- 商户归属查询所需的两个部分索引。
-- 非事务迁移（_notx）：CREATE INDEX CONCURRENTLY 不可在事务内执行，
-- 且可避免在 groups/accounts 热表上持 SHARE 锁阻塞读写。
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_groups_merchant_id
  ON groups(merchant_id) WHERE merchant_id IS NOT NULL;

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_accounts_merchant_id
  ON accounts(merchant_id) WHERE merchant_id IS NOT NULL;
