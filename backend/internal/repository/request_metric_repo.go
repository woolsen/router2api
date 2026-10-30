package repository

import (
	"context"
	"time"

	dbent "github.com/woolsen/router2api/ent"
	"github.com/woolsen/router2api/internal/service"
)

type requestMetricRepository struct {
	client *dbent.Client
}

// NewRequestMetricRepository 创建请求指标仓库。
func NewRequestMetricRepository(client *dbent.Client) service.RequestMetricRepository {
	return &requestMetricRepository{client: client}
}

// InsertRequestMetrics 批量写入请求指标。created_at 在此显式赋值，
// 表结构没有 updated_at，也不使用 TimeMixin。
func (r *requestMetricRepository) InsertRequestMetrics(ctx context.Context, metrics []service.RequestMetric) error {
	if len(metrics) == 0 {
		return nil
	}
	client := clientFromContext(ctx, r.client)
	builders := make([]*dbent.RequestMetricCreate, 0, len(metrics))
	now := time.Now().UTC()
	for _, metric := range metrics {
		builder := client.RequestMetric.Create().
			SetRequestID(metric.RequestID).
			SetUserID(metric.UserID).
			SetAPIKeyID(metric.APIKeyID).
			SetGroupID(metric.GroupID).
			SetModel(metric.Model).
			SetStatusCode(metric.StatusCode).
			SetUpstreamError(metric.UpstreamError).
			SetDurationMs(metric.DurationMS).
			SetFirstTokenMs(metric.FirstTokenMS).
			SetInputTokens(metric.InputTokens).
			SetOutputTokens(metric.OutputTokens).
			SetCacheReadTokens(metric.CacheReadTokens).
			SetCacheCreationTokens(metric.CacheCreationTokens).
			SetCreatedAt(now)
		if metric.UpstreamRequestID != "" {
			builder.SetUpstreamRequestID(metric.UpstreamRequestID)
		}
		if metric.MerchantID != nil {
			builder.SetMerchantID(*metric.MerchantID)
		}
		builders = append(builders, builder)
	}
	return client.RequestMetric.CreateBulk(builders...).Exec(ctx)
}
