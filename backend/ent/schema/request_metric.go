package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// RequestMetric 记录每次已鉴权网关请求的结果（成功与失败各一行）。
//
// usage_logs 只记录成功请求，无法计算分组成败率；该表与 usage_logs
// 通过 request_id 关联，失败请求没有 usage_logs 时也能独立成行。
type RequestMetric struct {
	ent.Schema
}

// Annotations 返回 schema 的注解配置。
func (RequestMetric) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "request_metrics"},
	}
}

// Fields 定义请求指标的所有字段。
//
// 该表只有 created_at，没有 updated_at，因此不使用 TimeMixin。
func (RequestMetric) Fields() []ent.Field {
	return []ent.Field{
		field.String("request_id").
			MaxLen(64).
			NotEmpty(),
		field.String("upstream_request_id").
			MaxLen(128).
			Optional().
			Nillable().
			Comment("上游响应声明的请求 ID；NULL 表示未提供"),
		field.Int64("user_id"),
		field.Int64("api_key_id").
			Default(0),
		field.Int64("group_id"),
		field.Int64("merchant_id").
			Optional().
			Nillable().
			Comment("分组商户快照；NULL 表示平台自营"),
		field.String("model").
			MaxLen(128).
			Default(""),
		field.Int("status_code").
			Default(0).
			Comment("最终写出的 HTTP 状态码；0 表示超时/未写出响应"),
		field.Bool("upstream_error").
			Default(false).
			Comment("是否归因于上游：5xx 或 status_code=0"),
		field.Int("duration_ms").
			Default(0),
		field.Int("first_token_ms").
			Default(0),
		field.Int("input_tokens").
			Default(0),
		field.Int("output_tokens").
			Default(0),
		field.Int("cache_read_tokens").
			Default(0),
		field.Int("cache_creation_tokens").
			Default(0),
		field.Time("created_at").
			Default(time.Now).
			Immutable().
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
	}
}

// Indexes 定义请求指标查询索引。
func (RequestMetric) Indexes() []ent.Index {
	return []ent.Index{
		// 分组维度的成功率 / TTFT 聚合按时间倒序扫描。
		index.Fields("group_id", "created_at").
			Annotations(entsql.DescColumns("created_at")),
		index.Fields("request_id"),
	}
}
