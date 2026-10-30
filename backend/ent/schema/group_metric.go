package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"

	"github.com/woolsen/router2api/ent/schema/mixins"
)

// GroupMetric 是分组按整点小时聚合的请求指标。
//
// 聚合源是 request_metrics；一行代表 (group_id, hour) 的唯一桶，重算时按
// 唯一索引 upsert 覆盖。计数与 token 总量保留原始可加值，比率只是派生值。
type GroupMetric struct {
	ent.Schema
}

// Annotations 返回 schema 的注解配置。
func (GroupMetric) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "group_metric_hourly"},
	}
}

// Mixin 使用标准 created_at / updated_at；DDL 与它保持一致。
func (GroupMetric) Mixin() []ent.Mixin {
	return []ent.Mixin{
		mixins.TimeMixin{},
	}
}

// Fields 定义小时聚合的所有字段。
func (GroupMetric) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("group_id"),
		field.Time("hour").
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}).
			Comment("聚合桶起点（UTC 整点）"),
		field.Int("requests").
			Default(0).
			Comment("该小时请求总数"),
		field.Int("upstream_failures").
			Default(0).
			Comment("归因于上游的失败数；4xx 用户错误不在此列"),
		field.Int("request_errors").
			Default(0).
			Comment("4xx 用户错误数，单独展示"),
		field.Int("ttft_samples").
			Default(0).
			Comment("有首包时间的流式样本数（分位数分母）"),
		field.Int64("input_tokens").
			Default(0),
		field.Int64("cache_read_tokens").
			Default(0),
		field.Int64("cache_creation_tokens").
			Default(0),
		field.Float("p50_ttft").
			Default(0).
			SchemaType(map[string]string{dialect.Postgres: "double precision"}).
			Comment("流式首包 P50（毫秒）"),
		field.Float("p95_ttft").
			Default(0).
			SchemaType(map[string]string{dialect.Postgres: "double precision"}).
			Comment("流式首包 P95（毫秒）"),
		field.Float("cache_hit_rate").
			Default(0).
			SchemaType(map[string]string{dialect.Postgres: "double precision"}).
			Comment("cache_read / (input + cache_read + cache_creation)"),
	}
}

// Indexes 定义小时桶的唯一约束与时间扫描索引。
func (GroupMetric) Indexes() []ent.Index {
	return []ent.Index{
		// 唯一索引是重算幂等的最后一道防线。
		index.Fields("group_id", "hour").Unique(),
		index.Fields("hour"),
	}
}
