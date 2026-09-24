package service

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUsageTrafficCounterSnapshotAndContextResolution(t *testing.T) {
	counter := NewUsageTrafficCounter()
	counter.AddIngressBodyBytes(11)
	counter.AddUpstreamRequestBodyBytes(12)
	counter.AddUpstreamResponseBodyBytes(13)
	counter.AddEgressBodyBytes(14)

	ctx := WithUsageTrafficCounter(context.Background(), counter)
	require.Equal(t, UsageTraffic{
		IngressBodyBytes:          11,
		UpstreamRequestBodyBytes:  12,
		UpstreamResponseBodyBytes: 13,
		EgressBodyBytes:           14,
	}, ResolveUsageTraffic(ctx, UsageTraffic{}))

	explicit := UsageTraffic{IngressBodyBytes: 99}
	require.Equal(t, explicit, ResolveUsageTraffic(ctx, explicit))
}

func TestUsageTrafficReadCloserCountsReadBytes(t *testing.T) {
	body := &trackedTestReadCloser{reader: bytes.NewBufferString("payload")}
	var read int
	wrapped := WrapUsageTrafficReadCloser(body, func(n int) { read += n })

	data, err := io.ReadAll(wrapped)
	require.NoError(t, err)
	require.Equal(t, "payload", string(data))
	require.Equal(t, len("payload"), read)
}

type trackedTestReadCloser struct {
	reader *bytes.Buffer
}

func (r *trackedTestReadCloser) Read(p []byte) (int, error)  { return r.reader.Read(p) }
func (r *trackedTestReadCloser) Close() error                { return nil }
