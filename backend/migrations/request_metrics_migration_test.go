//go:build unit

package migrations

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMigration253CreatesRequestMetrics(t *testing.T) {
	content, err := FS.ReadFile("253_request_metrics.sql")
	require.NoError(t, err)

	sql := string(content)
	require.Contains(t, sql, "CREATE TABLE IF NOT EXISTS request_metrics")
	require.Contains(t, sql, "request_id VARCHAR(64) NOT NULL")
	require.Contains(t, sql, "upstream_request_id VARCHAR(128)")
	require.Contains(t, sql, "user_id BIGINT NOT NULL")
	require.Contains(t, sql, "api_key_id BIGINT NOT NULL DEFAULT 0")
	require.Contains(t, sql, "group_id BIGINT NOT NULL")
	require.Contains(t, sql, "merchant_id BIGINT")
	require.Contains(t, sql, "model VARCHAR(128) NOT NULL DEFAULT ''")
	require.Contains(t, sql, "status_code INT NOT NULL DEFAULT 0")
	require.Contains(t, sql, "upstream_error BOOLEAN NOT NULL DEFAULT false")
	require.Contains(t, sql, "duration_ms INT NOT NULL DEFAULT 0")
	require.Contains(t, sql, "first_token_ms INT NOT NULL DEFAULT 0")
	require.Contains(t, sql, "input_tokens INT NOT NULL DEFAULT 0")
	require.Contains(t, sql, "output_tokens INT NOT NULL DEFAULT 0")
	require.Contains(t, sql, "cache_read_tokens INT NOT NULL DEFAULT 0")
	require.Contains(t, sql, "cache_creation_tokens INT NOT NULL DEFAULT 0")
	require.Contains(t, sql, "created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()")
	require.NotContains(t, sql, "updated_at")
	require.Contains(t, sql, "CREATE INDEX IF NOT EXISTS idx_request_metrics_group_created ON request_metrics(group_id, created_at DESC)")
	require.Contains(t, sql, "CREATE INDEX IF NOT EXISTS idx_request_metrics_request_id ON request_metrics(request_id)")
}
