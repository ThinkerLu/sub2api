package repository

import (
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestUsageLogUpstreamTransportIsIndependentOfRequestType(t *testing.T) {
	for _, transport := range []int16{0, 1, 2} {
		log := &service.UsageLog{RequestType: service.RequestTypeWSV2, UpstreamTransport: transport}
		prepared := prepareUsageLogInsert(log)
		require.Equal(t, int16(service.RequestTypeWSV2), prepared.args[30])
		require.Equal(t, transport, prepared.args[len(prepared.args)-3])
		require.Equal(t, "smallint", usageLogInsertArgTypes[len(usageLogInsertArgTypes)-3])
		require.Equal(t, service.RequestTypeWSV2, log.RequestType)
		require.True(t, log.OpenAIWSMode)
	}
}
func TestUsageLogUpstreamTransportFilter(t *testing.T) {
	v := int16(1)
	query, args := appendUpstreamTransportQueryFilter("SELECT 1 WHERE user_id = $1", []any{int64(2)}, &v, "ul")
	require.Equal(t, "SELECT 1 WHERE user_id = $1 AND ul.upstream_transport = $2", query)
	require.Equal(t, []any{int64(2), int16(1)}, args)
}
