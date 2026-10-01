package service

import (
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type dualLegErrorHTTPUpstream struct {
	HTTPUpstream
	event string
	calls atomic.Int32
}

func (u *dualLegErrorHTTPUpstream) Do(*http.Request, string, int64, int) (*http.Response, error) {
	u.calls.Add(1)
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader("data: " + u.event + "\n\n")),
	}, nil
}

func TestDualLegUpstreamErrorsPassThrough(t *testing.T) {
	for _, transport := range []string{"ws", "http"} {
		for _, tc := range []struct{ name, event string }{
			{"connection_limit", `{"type":"error","status":400,"error":{"type":"invalid_request_error","code":"websocket_connection_limit_reached","message":"Reconnect the websocket"},"request_id":"req_limit","sequence_number":7}`},
			{"missing_previous", `{"type":"error","error":{"type":"invalid_request_error","code":"previous_response_not_found","message":"Previous response not found"}}`},
			{"rate_limit", `{"type":"error","status":429,"error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"Please retry later"}}`},
			{"capacity_shed", `{"type":"error","error":{"code":"server_is_overloaded","message":"Upstream overloaded"}}`},
			{"slow_down", `{"type":"response.failed","response":{"id":"resp_slow","status":"failed","error":{"code":"slow_down","message":"Slow down"}}}`},
			{"failed_response", `{"type":"response.failed","response":{"id":"resp_failed","status":"failed","error":{"code":"server_error","message":"Upstream failed"},"usage":{"input_tokens":4,"output_tokens":2}}}`},
		} {
			t.Run(transport+"/"+tc.name, func(t *testing.T) {
				s, account, ws, httpUp := adaptiveFixture()
				upstream := &dualLegErrorHTTPUpstream{event: tc.event}
				if transport == "http" {
					s.cfg.Gateway.OpenAIWS.HTTPBridgeThresholdBytes = 1
					s.httpUpstream = upstream
				}
				server, _ := startAdaptiveServer(t, s, account, nil)
				client := dialAdaptive(t, server)
				writeAdaptive(t, client, `{"type":"response.create","model":"gpt-5.1","input":"hello"}`)
				if transport == "ws" {
					requirePassthroughUpstreamWrite(t, ws, time.Second)
					ws.Send(tc.event)
				}
				event, err := readPassthroughLifecycleFrame(t, client, time.Second)
				require.NoError(t, err)
				require.JSONEq(t, tc.event, string(event))
				require.Empty(t, ws.writes, "must not resubmit an upstream error")
				require.Empty(t, httpUp.snapshot(), "must not fallback after an upstream error")
				if transport == "http" {
					require.EqualValues(t, 1, upstream.calls.Load(), "must not resubmit the HTTP request")
				}
				_ = client.CloseNow()
			})
		}
	}
}

func TestGPT61SolUsesDualLegSizeThreshold(t *testing.T) {
	s, _, _, _ := adaptiveFixture()
	s.cfg.Gateway.OpenAIWS.HTTPBridgeThresholdBytes = 1024
	require.False(t, s.openAIWSRouteExceedsThreshold("gpt-6.1-sol", 1023))
	require.True(t, s.openAIWSRouteExceedsThreshold("gpt-6.1-sol", 1024))
	require.True(t, s.openAIWSRouteExceedsThreshold("gpt-6.1-sol", 100, 1024, 100))
}
