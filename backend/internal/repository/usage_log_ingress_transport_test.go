package repository

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestUsageLogIngressAndUpstreamTransportAreIndependent(t *testing.T) {
	for _, ingress := range []int16{0, 1, 2} {
		for _, upstream := range []int16{0, 1, 2} {
			log := &service.UsageLog{RequestType: service.RequestTypeWSV2, IngressTransport: ingress, UpstreamTransport: upstream}
			prepared := prepareUsageLogInsert(log)
			require.Equal(t, upstream, prepared.args[len(prepared.args)-3])
			require.Equal(t, ingress, prepared.args[len(prepared.args)-2])
			require.Equal(t, int16(service.RequestTypeWSV2), prepared.args[30])
		}
	}
}

func TestUsageTransportPairSQLDoesNotGuessIngressFromRequestType(t *testing.T) {
	ingress, upstream := int16(1), int16(2)
	query, args := appendUpstreamTransportQueryFilter("SELECT 1 WHERE TRUE", nil, &upstream, "ul")
	query, args = appendIngressTransportQueryFilter(query, args, &ingress, "ul")
	require.Equal(t, "SELECT 1 WHERE TRUE AND ul.upstream_transport = $1 AND ul.ingress_transport = $2", query)
	require.Equal(t, []any{int16(2), int16(1)}, args)
	require.NotContains(t, query, "request_type")
}
