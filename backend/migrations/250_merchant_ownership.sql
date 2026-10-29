-- 商户归属：分组与账号可绑定入驻商户；NULL 表示平台自营。
ALTER TABLE groups   ADD COLUMN IF NOT EXISTS merchant_id BIGINT;
ALTER TABLE accounts ADD COLUMN IF NOT EXISTS merchant_id BIGINT;
CREATE INDEX IF NOT EXISTS idx_groups_merchant_id   ON groups(merchant_id)   WHERE merchant_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_accounts_merchant_id ON accounts(merchant_id) WHERE merchant_id IS NOT NULL;
