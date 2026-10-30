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

// Withdrawal 是商户的提现申请。
// 金额沿用 merchant_balance 的 decimal(20,10)；申请时从 balance 转入 frozen，
// 驳回时反向解冻，打款时只释放 frozen。
type Withdrawal struct {
	ent.Schema
}

func (Withdrawal) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "withdrawals"},
	}
}

func (Withdrawal) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("merchant_id").
			Comment("商户 ID"),
		field.Float("amount").
			SchemaType(map[string]string{dialect.Postgres: "decimal(20,10)"}).
			Comment("提现金额"),
		field.String("status").
			MaxLen(20).
			Default("requested").
			Comment("状态：requested 待审核，approved 待打款，paid 已打款，rejected 已驳回"),
		field.String("proof_path").
			MaxLen(500).
			Optional().
			Nillable().
			Comment("打款凭证存储路径"),
		field.String("proof_sha256").
			MaxLen(64).
			Optional().
			Nillable().
			Comment("打款凭证 SHA-256"),
		field.String("approved_by").
			MaxLen(100).
			Optional().
			Nillable().
			Comment("审核/打款操作人"),
		field.Time("requested_at").
			Default(time.Now).
			Immutable().
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}).
			Comment("申请时间"),
		field.Time("paid_at").
			Optional().
			Nillable().
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}).
			Comment("打款完成时间"),
	}
}

func (Withdrawal) Indexes() []ent.Index {
	return []ent.Index{
		// 同一商户同时只能有一笔在途提现（requested/approved）。
		index.Fields("merchant_id").
			Unique().
			StorageKey("idx_withdrawals_open").
			Annotations(entsql.IndexWhere("status IN ('requested','approved')")),
		index.Fields("merchant_id", "requested_at"),
		index.Fields("status"),
	}
}
