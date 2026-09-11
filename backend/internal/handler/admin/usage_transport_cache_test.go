package admin

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestUsageCacheSeparatesUpstreamTransport(t *testing.T) {
	filters := usagestats.UsageLogFilters{}
	unknown := usageStatsCacheKey(filters)
	httpTransport := int16(1)
	filters.UpstreamTransport = &httpTransport
	httpKey := usageStatsCacheKey(filters)
	wsTransport := int16(2)
	filters.UpstreamTransport = &wsTransport
	require.NotEqual(t, unknown, httpKey)
	require.NotEqual(t, httpKey, usageStatsCacheKey(filters))
}
