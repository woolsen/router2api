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

// Settlement 是商户按周结算的应付单（T+7）。
// 金额字段沿用 usage_logs.actual_cost 的 decimal(20,10)，避免汇总时截断。
// 周期时间统一存 UTC，周一 00:00 为边界。
type Settlement struct {
	ent.Schema
}

func (Settlement) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "settlements"},
	}
}

func (Settlement) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("merchant_id").
			Comment("商户 ID"),
		field.Time("period_start").
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}).
			Comment("账期开始（UTC，周一 00:00）"),
		field.Time("period_end").
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}).
			Comment("账期结束（UTC，下周一 00:00）"),
		field.Float("gross_cost").
			Default(0).
			SchemaType(map[string]string{dialect.Postgres: "decimal(20,10)"}).
			Comment("用户实付合计"),
		field.Float("commission").
			Default(0).
			SchemaType(map[string]string{dialect.Postgres: "decimal(20,10)"}).
			Comment("平台抽成合计"),
		field.Float("net_payable").
			Default(0).
			SchemaType(map[string]string{dialect.Postgres: "decimal(20,10)"}).
			Comment("应付商户金额 = gross - commission"),
		field.String("status").
			MaxLen(20).
			Default("pending").
			Comment("状态：pending 待确认，confirmed 已确认，disputed 申诉中，paid 已打款"),
		field.Time("created_at").
			Default(time.Now).
			Immutable().
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now).
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
	}
}

func (Settlement) Indexes() []ent.Index {
	return []ent.Index{
		// 生成幂等的最后一道防线：同一商户同一账期只允许一张结算单。
		index.Fields("merchant_id", "period_start", "period_end").Unique(),
		index.Fields("status"),
	}
}
