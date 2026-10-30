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

// Dispute 是商户/用户对账目发起的申诉。
// 商户只能申诉自己的结算单；用户只能申诉自己的单条 usage_logs。
// 裁决结果写入 decision / adjust_amount，资金动作由 merchant_ledger 留痕。
type Dispute struct {
	ent.Schema
}

func (Dispute) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "disputes"},
	}
}

func (Dispute) Fields() []ent.Field {
	return []ent.Field{
		field.String("owner_type").
			MaxLen(20).
			Comment("merchant 商户 / user 普通用户"),
		field.Int64("owner_id").
			Comment("发起方 ID：merchants.id 或 users.id"),
		field.String("target_type").
			MaxLen(20).
			Comment("settlement 结算单 / usage 单条 usage_logs"),
		field.Int64("target_id"),
		field.Float("amount").
			Default(0).
			SchemaType(map[string]string{dialect.Postgres: "decimal(20,10)"}).
			Comment("主张的差异金额"),
		field.String("reason").
			SchemaType(map[string]string{dialect.Postgres: "text"}).
			Comment("申诉原因"),
		field.String("status").
			MaxLen(20).
			Default("open").
			Comment("open 待裁决 / resolved 已裁决"),
		field.String("decision").
			MaxLen(20).
			Optional().
			Nillable().
			Comment("reject 驳回 / confirm 认可 / adjust 调账"),
		field.Float("adjust_amount").
			Default(0).
			SchemaType(map[string]string{dialect.Postgres: "decimal(20,10)"}).
			Comment("调账增量：负数扣减，正数补足"),
		field.String("note").
			Optional().
			Nillable().
			SchemaType(map[string]string{dialect.Postgres: "text"}).
			Comment("裁决说明"),
		field.String("resolved_by").
			MaxLen(100).
			Optional().
			Nillable().
			Comment("裁决操作人"),
		field.Time("created_at").
			Default(time.Now).
			Immutable().
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.Time("resolved_at").
			Optional().
			Nillable().
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
	}
}

func (Dispute) Indexes() []ent.Index {
	return []ent.Index{
		// 同一目标同时只能有一条 open 申诉；裁决后允许重新发起。
		index.Fields("target_type", "target_id").
			Unique().
			StorageKey("idx_disputes_open_target").
			Annotations(entsql.IndexWhere("status = 'open'")),
		index.Fields("owner_type", "owner_id", "created_at").
			StorageKey("idx_disputes_owner_created"),
		index.Fields("status", "created_at").
			StorageKey("idx_disputes_status_created"),
	}
}
