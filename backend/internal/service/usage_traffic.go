package service

import (
	"context"
	"io"
	"sync/atomic"
)

// UsageTraffic stores application-layer body bytes for one usage-log record.
// It intentionally excludes HTTP headers, WebSocket framing, TLS records and
// TCP retransmissions, so it is stable and attributable per request or WS turn.
type UsageTraffic struct {
	IngressBodyBytes          int64 `json:"ingress_body_bytes"`
	UpstreamRequestBodyBytes  int64 `json:"upstream_request_body_bytes"`
	UpstreamResponseBodyBytes int64 `json:"upstream_response_body_bytes"`
	EgressBodyBytes           int64 `json:"egress_body_bytes"`
}

func (t UsageTraffic) IsZero() bool {
	return t.IngressBodyBytes == 0 &&
		t.UpstreamRequestBodyBytes == 0 &&
		t.UpstreamResponseBodyBytes == 0 &&
		t.EgressBodyBytes == 0
}

// UsageTrafficCounter is shared through a request context while HTTP bodies and
// request-scoped WebSocket frames are processed.
type UsageTrafficCounter struct {
	ingressBodyBytes          atomic.Int64
	upstreamRequestBodyBytes  atomic.Int64
	upstreamResponseBodyBytes atomic.Int64
	egressBodyBytes           atomic.Int64
}

func NewUsageTrafficCounter() *UsageTrafficCounter {
	return &UsageTrafficCounter{}
}

func (c *UsageTrafficCounter) AddIngressBodyBytes(n int) {
	if c != nil && n > 0 {
		c.ingressBodyBytes.Add(int64(n))
	}
}

func (c *UsageTrafficCounter) AddUpstreamRequestBodyBytes(n int) {
	if c != nil && n > 0 {
		c.upstreamRequestBodyBytes.Add(int64(n))
	}
}

func (c *UsageTrafficCounter) AddUpstreamResponseBodyBytes(n int) {
	if c != nil && n > 0 {
		c.upstreamResponseBodyBytes.Add(int64(n))
	}
}

func (c *UsageTrafficCounter) AddEgressBodyBytes(n int) {
	if c != nil && n > 0 {
		c.egressBodyBytes.Add(int64(n))
	}
}

func (c *UsageTrafficCounter) Snapshot() UsageTraffic {
	if c == nil {
		return UsageTraffic{}
	}
	return UsageTraffic{
		IngressBodyBytes:          c.ingressBodyBytes.Load(),
		UpstreamRequestBodyBytes:  c.upstreamRequestBodyBytes.Load(),
		UpstreamResponseBodyBytes: c.upstreamResponseBodyBytes.Load(),
		EgressBodyBytes:           c.egressBodyBytes.Load(),
	}
}

type usageTrafficContextKey struct{}

func WithUsageTrafficCounter(ctx context.Context, counter *UsageTrafficCounter) context.Context {
	if ctx == nil || counter == nil {
		return ctx
	}
	return context.WithValue(ctx, usageTrafficContextKey{}, counter)
}

func UsageTrafficCounterFromContext(ctx context.Context) *UsageTrafficCounter {
	if ctx == nil {
		return nil
	}
	counter, _ := ctx.Value(usageTrafficContextKey{}).(*UsageTrafficCounter)
	return counter
}

func ResolveUsageTraffic(ctx context.Context, explicit UsageTraffic) UsageTraffic {
	if !explicit.IsZero() {
		return explicit
	}
	return UsageTrafficCounterFromContext(ctx).Snapshot()
}

type usageTrafficReadCloser struct {
	io.ReadCloser
	onRead func(int)
}

func (r *usageTrafficReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if r.onRead != nil {
		r.onRead(n)
	}
	return n, err
}

func WrapUsageTrafficReadCloser(body io.ReadCloser, onRead func(int)) io.ReadCloser {
	if body == nil || onRead == nil {
		return body
	}
	return &usageTrafficReadCloser{ReadCloser: body, onRead: onRead}
}
