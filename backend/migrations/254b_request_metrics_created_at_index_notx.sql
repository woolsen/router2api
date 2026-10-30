-- 254b: request_metrics(created_at) 索引（非事务迁移）。
--
-- 小时聚合按 created_at 做全分组范围扫描；Task 1 的 (group_id, created_at)
-- 复合索引前缀是 group_id，无法服务全分组的整点聚合。
--
-- request_metrics 是热表，普通 CREATE INDEX 会在建索引期间阻塞写入，因此必须
-- CONCURRENTLY。CONCURRENTLY 不能在事务内执行，所以单独放在 *_notx.sql；
-- 新表与唯一索引仍留在 254_group_metrics.sql。
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_request_metrics_created
    ON request_metrics(created_at);
