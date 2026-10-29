package service

import (
	"context"

	"github.com/woolsen/router2api/internal/pkg/logger"
)

// merchantCommissionSource 提供商户配置；生产实现是 MerchantRepository。
// 它只在写入用量行时被读一次，用于固化当时的抽成率，结算流程不得再调用它。
type merchantCommissionSource interface {
	Get(ctx context.Context, id int64) (*Merchant, error)
}

// applyMerchantSnapshot 把分组归属的商户及其当前抽成率写入用量行。
// 平台自营分组（MerchantID == nil）保持 nil / 0，且不产生任何商户查询。
//
// 快照查询刻意不受请求 ctx 取消影响：用量行本身在 detached ctx 上落库，
// 若这里因客户端断开而放弃查询，就会落出一条没有商户归属的账单行，结算时少付商户。
func applyMerchantSnapshot(ctx context.Context, source merchantCommissionSource, usageLog *UsageLog, group *Group, component string) {
	if usageLog == nil || group == nil || group.MerchantID == nil {
		return
	}
	merchantID := *group.MerchantID
	if merchantID <= 0 {
		return
	}
	if source == nil {
		logger.LegacyPrintf(component,
			"Merchant commission source is not configured; usage row has no merchant snapshot (merchant=%d group=%d)", merchantID, group.ID)
		return
	}
	lookupCtx := ctx
	if lookupCtx == nil {
		lookupCtx = context.Background()
	} else {
		lookupCtx = context.WithoutCancel(lookupCtx)
	}
	merchant, err := source.Get(lookupCtx, merchantID)
	if err != nil || merchant == nil {
		// 查不到商户就不写 merchant_id：宁可让这一行留在待补结清单里，也不能写成
		// commission_rate=0（平台白送全额）。
		logger.LegacyPrintf(component,
			"Load merchant for usage snapshot failed (merchant=%d group=%d): %v", merchantID, group.ID, err)
		return
	}
	usageLog.MerchantID = &merchantID
	usageLog.CommissionRate = merchant.CommissionRate
}
