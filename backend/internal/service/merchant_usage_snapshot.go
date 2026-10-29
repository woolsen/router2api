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
// 平台自营分组（MerchantID == nil）保持 nil / nil，且不产生任何商户查询。
//
// 归属与费率分开写：分组上有 merchant_id 就一定要落库（否则这一行在结算里
// 等同于不存在），查不到商户时 merchant_id 保留、commission_rate 留 NULL，
// 由补结流程按 NULL 行处理；绝不能把“未知”写成 0 抽成（平台白送全额）。
//
// 快照查询刻意不受请求 ctx 取消影响：用量行本身在 detached ctx 上落库，
// 若这里因客户端断开而放弃查询，就会落出一条费率未知的账单行。
func applyMerchantSnapshot(ctx context.Context, source merchantCommissionSource, usageLog *UsageLog, group *Group, component string) {
	if usageLog == nil || group == nil || group.MerchantID == nil {
		return
	}
	merchantID := *group.MerchantID
	if merchantID <= 0 {
		return
	}
	// 归属先落：查商户失败也只影响费率，不影响这笔用量记在谁名下。
	usageLog.MerchantID = &merchantID
	if source == nil {
		logger.LegacyPrintf(component,
			"Merchant commission source is not configured; usage row keeps merchant_id without commission rate (merchant=%d group=%d)", merchantID, group.ID)
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
		logger.LegacyPrintf(component,
			"Load merchant for usage snapshot failed (merchant=%d group=%d): %v; commission_rate stays NULL for backfill", merchantID, group.ID, err)
		return
	}
	rate := merchant.CommissionRate
	usageLog.CommissionRate = &rate
}
