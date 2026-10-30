package repository

import (
	"context"
	"database/sql"
	"time"

	"github.com/lib/pq"

	"github.com/woolsen/router2api/internal/service"
)

// groupMetricAggregateQuery 从 request_metrics 汇总一个整点小时。
//
// 口径（与 M2 评审一致，改动前先看 Task 2 brief）：
//   - created_at 是 [from, to) 的半开区间，桶边界按 UTC 整点；
//   - upstream_failures 只算 upstream_error（5xx、未写出响应、带内流失败）；
//   - request_errors 单独统计 4xx 用户错误，绝不并入上游失败；
//   - TTFT 只统计 first_token_ms > 0 的流式样本；
//   - 缓存命中率的分母是 input + cache_read + cache_creation。
const groupMetricAggregateQuery = `
SELECT
    group_id,
    COUNT(*) AS requests,
    COUNT(*) FILTER (WHERE upstream_error) AS upstream_failures,
    COUNT(*) FILTER (WHERE status_code >= 400 AND status_code < 500 AND NOT upstream_error) AS request_errors,
    COUNT(*) FILTER (WHERE first_token_ms > 0) AS ttft_samples,
    COALESCE(SUM(input_tokens), 0) AS input_tokens,
    COALESCE(SUM(cache_read_tokens), 0) AS cache_read_tokens,
    COALESCE(SUM(cache_creation_tokens), 0) AS cache_creation_tokens,
    COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY first_token_ms)
        FILTER (WHERE first_token_ms > 0), 0) AS p50_ttft,
    COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY first_token_ms)
        FILTER (WHERE first_token_ms > 0), 0) AS p95_ttft
FROM request_metrics
WHERE created_at >= $1 AND created_at < $2
GROUP BY group_id
ORDER BY group_id`

// groupMetricUpsertQuery 按 (group_id, hour) 覆盖写入，保证同一小时可重算。
const groupMetricUpsertQuery = `
INSERT INTO group_metric_hourly (
    group_id, hour, requests, upstream_failures, request_errors, ttft_samples,
    input_tokens, cache_read_tokens, cache_creation_tokens,
    p50_ttft, p95_ttft, cache_hit_rate
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
ON CONFLICT (group_id, hour) DO UPDATE SET
    requests = EXCLUDED.requests,
    upstream_failures = EXCLUDED.upstream_failures,
    request_errors = EXCLUDED.request_errors,
    ttft_samples = EXCLUDED.ttft_samples,
    input_tokens = EXCLUDED.input_tokens,
    cache_read_tokens = EXCLUDED.cache_read_tokens,
    cache_creation_tokens = EXCLUDED.cache_creation_tokens,
    p50_ttft = EXCLUDED.p50_ttft,
    p95_ttft = EXCLUDED.p95_ttft,
    cache_hit_rate = EXCLUDED.cache_hit_rate,
    updated_at = NOW()`

const groupMetricListQuery = `
SELECT
    group_id, hour, requests, upstream_failures, request_errors, ttft_samples,
    input_tokens, cache_read_tokens, cache_creation_tokens,
    p50_ttft, p95_ttft, cache_hit_rate
FROM group_metric_hourly
WHERE group_id = ANY($1) AND hour >= $2 AND hour < $3
ORDER BY group_id, hour`

// groupHealthSampleQuery 一次读取健康判定所需的 1h 与 15m 两个窗口。
const groupHealthSampleQuery = `
SELECT
    COUNT(*) FILTER (WHERE created_at >= $2) AS requests,
    COUNT(*) FILTER (WHERE created_at >= $2 AND upstream_error) AS upstream_failures,
    COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY first_token_ms)
        FILTER (WHERE created_at >= $2 AND first_token_ms > 0), 0) AS p95_ttft,
    COUNT(*) FILTER (WHERE created_at >= $3) AS recent_requests,
    COUNT(*) FILTER (WHERE created_at >= $3 AND NOT upstream_error) AS recent_successes
FROM request_metrics
WHERE group_id = $1 AND created_at >= $2`

type groupMetricRepository struct {
	sql sqlExecutor
}

// NewGroupMetricRepository 创建分组小时指标仓库。
func NewGroupMetricRepository(sqlDB *sql.DB) service.GroupMetricRepository {
	return &groupMetricRepository{sql: sqlDB}
}

// newGroupMetricRepositoryWithSQL 用于只依赖 sqlExecutor 的单元测试。
func newGroupMetricRepositoryWithSQL(sqlq sqlExecutor) *groupMetricRepository {
	return &groupMetricRepository{sql: sqlq}
}

func (r *groupMetricRepository) AggregateRequestMetrics(
	ctx context.Context,
	from, to time.Time,
) ([]service.GroupMetric, error) {
	rows, err := r.sql.QueryContext(ctx, groupMetricAggregateQuery, from, to)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	metrics := make([]service.GroupMetric, 0)
	for rows.Next() {
		var (
			metric                              service.GroupMetric
			requests, upstreamFailures          int64
			requestErrors, ttftSamples          int64
			inputTokens, cacheRead, cacheCreate int64
		)
		if err := rows.Scan(
			&metric.GroupID,
			&requests,
			&upstreamFailures,
			&requestErrors,
			&ttftSamples,
			&inputTokens,
			&cacheRead,
			&cacheCreate,
			&metric.P50TTFT,
			&metric.P95TTFT,
		); err != nil {
			return nil, err
		}
		metric.Hour = from.UTC().Truncate(time.Hour)
		metric.Requests = int(requests)
		metric.UpstreamFailures = int(upstreamFailures)
		metric.RequestErrors = int(requestErrors)
		metric.TTFTSamples = int(ttftSamples)
		metric.InputTokens = inputTokens
		metric.CacheReadTokens = cacheRead
		metric.CacheCreationTokens = cacheCreate
		denominator := inputTokens + cacheRead + cacheCreate
		if denominator > 0 {
			metric.CacheHitRate = float64(cacheRead) / float64(denominator)
		}
		metrics = append(metrics, metric)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return metrics, nil
}

func (r *groupMetricRepository) UpsertGroupMetrics(
	ctx context.Context,
	hour time.Time,
	metrics []service.GroupMetric,
) error {
	hour = hour.UTC().Truncate(time.Hour)
	for _, metric := range metrics {
		if _, err := r.sql.ExecContext(
			ctx,
			groupMetricUpsertQuery,
			metric.GroupID,
			hour,
			metric.Requests,
			metric.UpstreamFailures,
			metric.RequestErrors,
			metric.TTFTSamples,
			metric.InputTokens,
			metric.CacheReadTokens,
			metric.CacheCreationTokens,
			metric.P50TTFT,
			metric.P95TTFT,
			metric.CacheHitRate,
		); err != nil {
			return err
		}
	}
	return nil
}

func (r *groupMetricRepository) ListGroupMetrics(
	ctx context.Context,
	groupIDs []int64,
	from, to time.Time,
) ([]service.GroupMetric, error) {
	if len(groupIDs) == 0 {
		return nil, nil
	}
	rows, err := r.sql.QueryContext(ctx, groupMetricListQuery, pq.Array(groupIDs), from, to)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	metrics := make([]service.GroupMetric, 0)
	for rows.Next() {
		var metric service.GroupMetric
		if err := rows.Scan(
			&metric.GroupID,
			&metric.Hour,
			&metric.Requests,
			&metric.UpstreamFailures,
			&metric.RequestErrors,
			&metric.TTFTSamples,
			&metric.InputTokens,
			&metric.CacheReadTokens,
			&metric.CacheCreationTokens,
			&metric.P50TTFT,
			&metric.P95TTFT,
			&metric.CacheHitRate,
		); err != nil {
			return nil, err
		}
		metric.Hour = metric.Hour.UTC().Truncate(time.Hour)
		metrics = append(metrics, metric)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return metrics, nil
}

func (r *groupMetricRepository) GroupHealthSample(
	ctx context.Context,
	groupID int64,
	since, recentSince time.Time,
) (service.GroupHealthSample, error) {
	var (
		sample                          service.GroupHealthSample
		requests, upstreamFailures      int64
		recentRequests, recentSuccesses int64
	)
	if err := scanSingleRow(
		ctx,
		r.sql,
		groupHealthSampleQuery,
		[]any{groupID, since, recentSince},
		&requests,
		&upstreamFailures,
		&sample.P95TTFT,
		&recentRequests,
		&recentSuccesses,
	); err != nil {
		return service.GroupHealthSample{}, err
	}
	sample.Requests = int(requests)
	sample.UpstreamFailures = int(upstreamFailures)
	sample.RecentRequests = int(recentRequests)
	sample.RecentSuccesses = int(recentSuccesses)
	return sample, nil
}
