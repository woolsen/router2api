//go:build unit

package migrations

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMigration255AddsMerchantRoleLink(t *testing.T) {
	content, err := FS.ReadFile("255_merchant_role.sql")
	require.NoError(t, err)

	sql := string(content)
	require.Contains(t, sql, "ALTER TABLE users ADD COLUMN IF NOT EXISTS merchant_id BIGINT")
	require.Contains(t, sql, "CREATE INDEX IF NOT EXISTS idx_users_merchant_id")
	require.Contains(t, sql, "ON users(merchant_id) WHERE merchant_id IS NOT NULL")
	require.Contains(t, sql, "COMMENT ON COLUMN users.merchant_id")
}
