package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUsageIngressTransportMigrationKeepsHistoricalUnknown(t *testing.T) {
	content, err := FS.ReadFile("242_usage_log_ingress_transport.sql")
	require.NoError(t, err)
	sql := strings.Join(strings.Fields(string(content)), " ")
	require.Contains(t, sql, "ADD COLUMN IF NOT EXISTS ingress_transport SMALLINT NOT NULL DEFAULT 0")
	require.NotContains(t, strings.ToUpper(sql), "UPDATE USAGE_LOGS")
	require.NotContains(t, strings.ToUpper(sql), "DROP COLUMN")
}
