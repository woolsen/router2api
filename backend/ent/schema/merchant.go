package schema

import (
	"github.com/woolsen/router2api/ent/schema/mixins"

	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// Merchant holds the schema definition for the Merchant entity.
// 商户（第三方中转站）是市场上游提供方，通过抽成比例与平台结算。
type Merchant struct {
	ent.Schema
}

func (Merchant) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "merchants"},
	}
}

func (Merchant) Mixin() []ent.Mixin {
	return []ent.Mixin{
		mixins.TimeMixin{},
		mixins.SoftDeleteMixin{},
	}
}

func (Merchant) Fields() []ent.Field {
	return []ent.Field{
		field.String("name").
			MaxLen(100).
			NotEmpty().
			Comment("商户名称"),
		field.String("contact").
			MaxLen(200).
			Optional().
			Nillable().
			Comment("联系方式"),
		field.String("status").
			MaxLen(20).
			Default("active").
			Comment("状态：active 正常，suspended 暂停"),
		field.Float("commission_rate").
			SchemaType(map[string]string{dialect.Postgres: "decimal(10,4)"}).
			Default(0.08).
			Comment("平台抽成比例，小数（0.08=8%）"),
	}
}

func (Merchant) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("status"),
		index.Fields("deleted_at"),
	}
}
