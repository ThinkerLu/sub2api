package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUsageLogTrafficBytesMigration(t *testing.T) {
	content, err := FS.ReadFile("241_usage_log_traffic_bytes.sql")
	require.NoError(t, err)

	sql := strings.Join(strings.Fields(string(content)), " ")
	for _, column := range []string{
		"ingress_body_bytes",
		"upstream_request_body_bytes",
		"upstream_response_body_bytes",
		"egress_body_bytes",
	} {
		require.Contains(t, sql, "ADD COLUMN IF NOT EXISTS "+column+" BIGINT NOT NULL DEFAULT 0")
	}
	require.Contains(t, sql, "CREATE INDEX IF NOT EXISTS idx_usage_logs_session_id_exact")
	require.Contains(t, sql, "ALTER TABLE usage_dashboard_hourly")
	require.Contains(t, sql, "ALTER TABLE usage_dashboard_daily")
	require.NotContains(t, strings.ToUpper(sql), "DROP COLUMN")
}
