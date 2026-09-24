package handler

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/stretchr/testify/require"
)

func TestUsageRecordContextPreservesTrafficCounter(t *testing.T) {
	counter := service.NewUsageTrafficCounter()
	counter.AddIngressBodyBytes(42)
	parent := service.WithUsageTrafficCounter(context.Background(), counter)

	ctx := usageRecordContext(parent, context.Background())
	require.Equal(t, int64(42), service.UsageTrafficCounterFromContext(ctx).Snapshot().IngressBodyBytes)
}
