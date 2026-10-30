-- 商户角色：users.role = 'merchant' 的账号通过 users.merchant_id 绑定入驻商户。
-- 角色本身是 VARCHAR(20)，无需改列；这里只加归属列。
-- 索引使用部分索引：绝大多数用户不是商户，NULL 行不必进索引。
ALTER TABLE users ADD COLUMN IF NOT EXISTS merchant_id BIGINT;

CREATE INDEX IF NOT EXISTS idx_users_merchant_id
  ON users(merchant_id) WHERE merchant_id IS NOT NULL;

COMMENT ON COLUMN users.merchant_id IS '所属商户；NULL 表示非商户账号';
