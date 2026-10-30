//go:build unit

package migrations

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMigration254CreatesGroupMetricHourly(t *testing.T) {
	content, err := FS.ReadFile("254_group_metrics.sql")
	require.NoError(t, err)

	sql := string(content)
	require.Contains(t, sql, "CREATE TABLE IF NOT EXISTS group_metric_hourly")
	require.Contains(t, sql, "group_id BIGINT NOT NULL")
	require.Contains(t, sql, "hour TIMESTAMPTZ NOT NULL")
	require.Contains(t, sql, "requests INT NOT NULL DEFAULT 0")
	require.Contains(t, sql, "upstream_failures INT NOT NULL DEFAULT 0")
	require.Contains(t, sql, "request_errors INT NOT NULL DEFAULT 0")
	require.Contains(t, sql, "ttft_samples INT NOT NULL DEFAULT 0")
	require.Contains(t, sql, "input_tokens BIGINT NOT NULL DEFAULT 0")
	require.Contains(t, sql, "cache_read_tokens BIGINT NOT NULL DEFAULT 0")
	require.Contains(t, sql, "cache_creation_tokens BIGINT NOT NULL DEFAULT 0")
	require.Contains(t, sql, "p50_ttft DOUBLE PRECISION NOT NULL DEFAULT 0")
	require.Contains(t, sql, "p95_ttft DOUBLE PRECISION NOT NULL DEFAULT 0")
	require.Contains(t, sql, "cache_hit_rate DOUBLE PRECISION NOT NULL DEFAULT 0")
	require.Contains(t, sql, "created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()")
	require.Contains(t, sql, "updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()")
	require.Contains(t, sql, "CREATE UNIQUE INDEX IF NOT EXISTS idx_group_metric_hourly_group_hour")
	require.Contains(t, sql, "ON group_metric_hourly(group_id, hour)")
	require.Contains(t, sql, "CREATE INDEX IF NOT EXISTS idx_request_metrics_created")
	require.Contains(t, sql, "ON request_metrics(created_at)")
}
