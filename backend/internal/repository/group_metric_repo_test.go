package repository

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"

	"github.com/woolsen/router2api/internal/service"
)

// 聚合 SQL 的口径每一条都要钉住：4xx 不能进上游失败、TTFT 只算有首包的样本。
func TestGroupMetricRepositoryAggregateRequestMetrics(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newGroupMetricRepositoryWithSQL(db)

	from := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)

	mock.ExpectQuery(`(?s)SELECT\s+group_id,\s+COUNT\(\*\) AS requests,\s+`+
		`COUNT\(\*\) FILTER \(WHERE upstream_error\) AS upstream_failures,\s+`+
		`COUNT\(\*\) FILTER \(WHERE status_code >= 400 AND status_code < 500 AND NOT upstream_error\) AS request_errors,\s+`+
		`COUNT\(\*\) FILTER \(WHERE first_token_ms > 0\) AS ttft_samples,.*`+
		`percentile_cont\(0.95\).*`+
		`FROM request_metrics\s+WHERE created_at >= \$1 AND created_at < \$2\s+GROUP BY group_id\s+ORDER BY group_id`,
	).
		WithArgs(from, to).
		WillReturnRows(sqlmock.NewRows([]string{
			"group_id", "requests", "upstream_failures", "request_errors", "ttft_samples",
			"input_tokens", "cache_read_tokens", "cache_creation_tokens", "p50_ttft", "p95_ttft",
		}).AddRow(int64(4), int64(20), int64(2), int64(1), int64(18),
			int64(100), int64(800), int64(100), 320.0, 950.0))

	metrics, err := repo.AggregateRequestMetrics(context.Background(), from, to)
	require.NoError(t, err)
	require.Len(t, metrics, 1)
	m := metrics[0]
	require.Equal(t, int64(4), m.GroupID)
	require.True(t, m.Hour.Equal(from))
	require.Equal(t, 20, m.Requests)
	require.Equal(t, 2, m.UpstreamFailures)
	require.Equal(t, 1, m.RequestErrors)
	require.Equal(t, 18, m.TTFTSamples)
	require.InDelta(t, 0.8, m.CacheHitRate, 1e-9)
	require.InDelta(t, 320.0, m.P50TTFT, 1e-9)
	require.InDelta(t, 950.0, m.P95TTFT, 1e-9)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGroupMetricRepositoryAggregateNoRows(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newGroupMetricRepositoryWithSQL(db)

	from := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`(?s)FROM request_metrics.*ORDER BY group_id`).
		WithArgs(from, from.Add(time.Hour)).
		WillReturnRows(sqlmock.NewRows([]string{"group_id"}))

	metrics, err := repo.AggregateRequestMetrics(context.Background(), from, from.Add(time.Hour))
	require.NoError(t, err)
	require.Empty(t, metrics)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGroupMetricRepositoryUpsertUsesOnConflictUpdate(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newGroupMetricRepositoryWithSQL(db)

	hour := time.Date(2026, 10, 29, 10, 45, 0, 0, time.UTC)
	metric := service.GroupMetric{
		GroupID:             7,
		Requests:            20,
		UpstreamFailures:    2,
		RequestErrors:       1,
		TTFTSamples:         18,
		InputTokens:         100,
		CacheReadTokens:     800,
		CacheCreationTokens: 100,
		P50TTFT:             320,
		P95TTFT:             950,
		CacheHitRate:        0.8,
	}

	mock.ExpectExec(`(?s)INSERT INTO group_metric_hourly.*ON CONFLICT \(group_id, hour\) DO UPDATE SET.*requests = EXCLUDED.requests.*updated_at = NOW\(\)`).
		WithArgs(int64(7), hour.Truncate(time.Hour), 20, 2, 1, 18, int64(100), int64(800), int64(100), 320.0, 950.0, 0.8).
		WillReturnResult(sqlmock.NewResult(1, 1))

	require.NoError(t, repo.UpsertGroupMetrics(context.Background(), hour, []service.GroupMetric{metric}))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGroupMetricRepositoryUpsertEmptyIsNoop(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newGroupMetricRepositoryWithSQL(db)
	require.NoError(t, repo.UpsertGroupMetrics(context.Background(), time.Now(), nil))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGroupMetricRepositoryListGroupMetrics(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newGroupMetricRepositoryWithSQL(db)

	from := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)
	hour := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)

	mock.ExpectQuery(`(?s)SELECT\s+group_id, hour, requests, upstream_failures, request_errors, ttft_samples,.*FROM group_metric_hourly\s+WHERE group_id = ANY\(\$1\) AND hour >= \$2 AND hour < \$3\s+ORDER BY group_id, hour`).
		WithArgs(pq.Array([]int64{3, 5}), from, to).
		WillReturnRows(sqlmock.NewRows([]string{
			"group_id", "hour", "requests", "upstream_failures", "request_errors", "ttft_samples",
			"input_tokens", "cache_read_tokens", "cache_creation_tokens", "p50_ttft", "p95_ttft", "cache_hit_rate",
		}).AddRow(int64(3), hour, int64(60), int64(5), int64(2), int64(50),
			int64(1000), int64(250), int64(0), 300.0, 900.0, 0.2))

	metrics, err := repo.ListGroupMetrics(context.Background(), []int64{3, 5}, from, to)
	require.NoError(t, err)
	require.Len(t, metrics, 1)
	require.Equal(t, int64(3), metrics[0].GroupID)
	require.True(t, metrics[0].Hour.Equal(hour))
	require.Equal(t, 60, metrics[0].Requests)
	require.InDelta(t, 0.2, metrics[0].CacheHitRate, 1e-9)
	require.NoError(t, mock.ExpectationsWereMet())

	// 空分组列表不能发查询。
	metrics, err = repo.ListGroupMetrics(context.Background(), nil, from, to)
	require.NoError(t, err)
	require.Nil(t, metrics)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGroupMetricRepositoryGroupHealthSamples(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := newGroupMetricRepositoryWithSQL(db)

	since := time.Date(2026, 10, 29, 11, 0, 0, 0, time.UTC)
	recentSince := time.Date(2026, 10, 29, 11, 45, 0, 0, time.UTC)

	mock.ExpectQuery(`(?s)SELECT\s+group_id,\s+`+
		`COUNT\(\*\) FILTER \(WHERE created_at >= \$2\) AS requests,\s+`+
		`COUNT\(\*\) FILTER \(WHERE created_at >= \$2 AND upstream_error\) AS upstream_failures,.*`+
		`COUNT\(\*\) FILTER \(WHERE created_at >= \$3\) AS recent_requests,\s+`+
		`COUNT\(\*\) FILTER \(WHERE created_at >= \$3 AND NOT upstream_error\) AS recent_successes\s+`+
		`FROM request_metrics\s+WHERE group_id = ANY\(\$1\) AND created_at >= \$2\s+GROUP BY group_id`).
		WithArgs(pq.Array([]int64{9, 10}), since, recentSince).
		WillReturnRows(sqlmock.NewRows([]string{
			"group_id", "requests", "upstream_failures", "p95_ttft", "recent_requests", "recent_successes",
		}).
			AddRow(int64(9), int64(25), int64(4), 1200.0, int64(6), int64(2)).
			AddRow(int64(10), int64(3), int64(3), 0.0, int64(3), int64(0)))

	samples, err := repo.GroupHealthSamples(context.Background(), []int64{9, 10}, since, recentSince)
	require.NoError(t, err)
	require.Len(t, samples, 2)

	sample := samples[9]
	require.Equal(t, 25, sample.Requests)
	require.Equal(t, 4, sample.UpstreamFailures)
	require.InDelta(t, 1200.0, sample.P95TTFT, 1e-9)
	require.Equal(t, 6, sample.RecentRequests)
	require.Equal(t, 2, sample.RecentSuccesses)

	require.Equal(t, 3, samples[10].Requests)
	require.Equal(t, 3, samples[10].UpstreamFailures)
	require.Zero(t, samples[10].RecentSuccesses)

	// 空分组列表不能发查询。
	samples, err = repo.GroupHealthSamples(context.Background(), nil, since, recentSince)
	require.NoError(t, err)
	require.Empty(t, samples)
	require.NoError(t, mock.ExpectationsWereMet())
}
