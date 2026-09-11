package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type adaptiveHTTPUpstream struct {
	mu       sync.Mutex
	requests [][]byte
}

func (u *adaptiveHTTPUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	_ = req.Body.Close()
	u.mu.Lock()
	u.requests = append(u.requests, body)
	n := len(u.requests)
	u.mu.Unlock()
	response := fmt.Sprintf(`{"id":"resp_http_%d","model":"gpt-5.1","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`, n)
	events := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"answer\"}\n\ndata: {\"type\":\"response.completed\",\"response\":" + response + "}\n\n"
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(events))}, nil
}
func (u *adaptiveHTTPUpstream) DoWithTLS(r *http.Request, p string, id int64, c int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(r, p, id, c)
}
func (u *adaptiveHTTPUpstream) snapshot() [][]byte {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([][]byte(nil), u.requests...)
}

func startAdaptiveServer(t *testing.T, s *OpenAIGatewayService, a *Account, hooks *OpenAIWSIngressHooks) (*httptest.Server, <-chan error) {
	t.Helper()
	errs := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, &coderws.AcceptOptions{CompressionMode: coderws.CompressionContextTakeover})
		if err != nil {
			errs <- err
			return
		}
		defer conn.CloseNow()
		if s.IsOpenAIWSAdaptive(a) {
			conn.SetReadLimit(-1)
		} else {
			conn.SetReadLimit(ResolveOpenAIWSClientReadLimitBytes(s.cfg))
		}
		ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
		defer cancel()
		_, first, err := conn.Read(ctx)
		if err != nil {
			errs <- err
			return
		}
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = r.Clone(ctx)
		c.Request.Header = r.Header.Clone()
		errs <- s.ProxyResponsesWebSocketFromClient(ctx, c, conn, a, "sk-test", first, hooks)
	}))
	t.Cleanup(server.Close)
	return server, errs
}
func adaptiveFixture() (*OpenAIGatewayService, *Account, *stagedPassthroughConn, *adaptiveHTTPUpstream) {
	cfg := passthroughLifecycleConfig()
	cfg.Gateway.OpenAIWS.HTTPBridgeThresholdBytes = 1024
	cfg.Gateway.OpenAIWS.HTTPBridgeEnabled = false
	ws := newStagedPassthroughConn()
	s := newPassthroughLifecycleService(cfg, ws)
	h := &adaptiveHTTPUpstream{}
	s.httpUpstream = h
	s.toolCorrector = NewCodexToolCorrector()
	a := passthroughLifecycleAccount()
	a.Extra["openai_apikey_responses_websockets_v2_mode"] = OpenAIWSIngressModeAdaptive
	return s, a, ws, h
}
func dialAdaptive(t *testing.T, server *httptest.Server) *coderws.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), &coderws.DialOptions{CompressionMode: coderws.CompressionContextTakeover})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.CloseNow() })
	return c
}
func writeAdaptive(t *testing.T, c *coderws.Conn, p string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	require.NoError(t, c.Write(ctx, coderws.MessageText, []byte(p)))
}
func readAdaptiveCompleted(t *testing.T, c *coderws.Conn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for {
		_, p, err := c.Read(ctx)
		require.NoError(t, err)
		if gjson.GetBytes(p, "type").String() == "response.completed" {
			return
		}
	}
}

func TestAdaptiveBridgeCompactionHandoff(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s, a, ws, httpUp := adaptiveFixture()
	var mu sync.Mutex
	var turns, before []int
	var bridge []bool
	hooks := &OpenAIWSIngressHooks{BeforeTurn: func(n int) error { mu.Lock(); defer mu.Unlock(); before = append(before, n); return nil }, AfterTurn: func(n int, r *OpenAIForwardResult, err error) {
		if err != nil {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		turns = append(turns, n)
		bridge = append(bridge, r.OpenAIWSHTTPBridge)
	}}
	server, errs := startAdaptiveServer(t, s, a, hooks)
	client := dialAdaptive(t, server)
	writeAdaptive(t, client, `{"type":"response.create","model":"gpt-5.1","input":[{"role":"user","content":"`+strings.Repeat("x", 2048)+`"}]}`)
	readAdaptiveCompleted(t, client)
	writeAdaptive(t, client, `{"type":"response.create","model":"gpt-5.1","previous_response_id":"resp_http_1","input":[{"role":"user","content":"continue"}]}`)
	readAdaptiveCompleted(t, client)
	requests := httpUp.snapshot()
	require.Len(t, requests, 2)
	require.False(t, gjson.GetBytes(requests[1], "previous_response_id").Exists())
	require.Contains(t, string(requests[1]), strings.Repeat("x", 2048))
	require.Contains(t, string(requests[1]), "answer")
	writeAdaptive(t, client, `{"type":"response.create","model":"gpt-5.1","input":[{"type":"compaction","encrypted_content":"compacted"},{"role":"user","content":"new"}]}`)
	sent := requirePassthroughUpstreamWrite(t, ws, 3*time.Second)
	require.Contains(t, string(sent), "compacted")
	require.NotContains(t, string(sent), strings.Repeat("x", 100))
	require.False(t, gjson.GetBytes(sent, "previous_response_id").Exists())
	ws.Send(`{"type":"response.completed","response":{"id":"resp_ws_3","model":"gpt-5.1","usage":{"input_tokens":1,"output_tokens":1}}}`)
	readAdaptiveCompleted(t, client)
	writeAdaptive(t, client, `{"type":"response.create","previous_response_id":"resp_ws_3","input":[{"role":"user","content":"last"}]}`)
	sent = requirePassthroughUpstreamWrite(t, ws, 3*time.Second)
	require.Equal(t, "resp_ws_3", gjson.GetBytes(sent, "previous_response_id").String())
	ws.Send(`{"type":"response.completed","response":{"id":"resp_ws_4","model":"gpt-5.1","usage":{"input_tokens":1,"output_tokens":1}}}`)
	readAdaptiveCompleted(t, client)
	require.Eventually(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(turns) == 4 }, time.Second, time.Millisecond)
	mu.Lock()
	require.Equal(t, []int{1, 2, 3, 4}, turns)
	require.Equal(t, []int{1, 2, 3, 4}, before)
	require.Equal(t, []bool{true, true, false, false}, bridge)
	mu.Unlock()
	require.Len(t, httpUp.snapshot(), 2)
	_ = client.CloseNow()
	select {
	case <-errs:
	case <-time.After(3 * time.Second):
		t.Fatal("session did not exit")
	}
}

func TestAdaptiveRejectsUnknownPreviousResponse(t *testing.T) {
	s, a, _, httpUp := adaptiveFixture()
	server, errs := startAdaptiveServer(t, s, a, nil)
	c := dialAdaptive(t, server)
	writeAdaptive(t, c, `{"type":"response.create","model":"gpt-5.1","previous_response_id":"resp_unknown","input":"delta"}`)
	select {
	case err := <-errs:
		require.ErrorContains(t, err, "unknown previous_response_id")
	case <-time.After(3 * time.Second):
		t.Fatal("no rejection")
	}
	require.Empty(t, httpUp.snapshot())
}

func TestAdaptiveHandoffReleasesHistoryAndOffsetsHooks(t *testing.T) {
	h := &openAIWSAdaptiveHandoff{turn: 3, history: []json.RawMessage{json.RawMessage(`"large history"`)}, payload: []byte(`{}`)}
	var turn int
	hooks := adaptivePassthroughHooks(&OpenAIWSIngressHooks{AfterTurn: func(n int, _ *OpenAIForwardResult, _ error) { turn = n }}, h)
	hooks.AfterTurn(1, &OpenAIForwardResult{OpenAIWSMode: true, UpstreamTerminalEvent: "response.completed"}, nil)
	require.Nil(t, h.history)
	require.Nil(t, h.payload)
	require.Equal(t, 3, turn)
}

func TestAdaptiveSmallFirstMessageUsesWS(t *testing.T) {
	s, a, ws, httpUp := adaptiveFixture()
	server, _ := startAdaptiveServer(t, s, a, nil)
	c := dialAdaptive(t, server)
	writeAdaptive(t, c, `{"type":"response.create","model":"gpt-5.1","input":"small"}`)
	got := requirePassthroughUpstreamWrite(t, ws, 3*time.Second)
	require.Equal(t, "small", gjson.GetBytes(got, "input").String())
	require.Empty(t, httpUp.snapshot())
	ws.Send(`{"type":"response.completed","response":{"id":"resp_small","model":"gpt-5.1","usage":{"input_tokens":1,"output_tokens":1}}}`)
	readAdaptiveCompleted(t, c)
}

func TestAdaptiveCompressedMessageAbove64MiB(t *testing.T) {
	if testing.Short() {
		t.Skip("large compressed websocket integration")
	}
	s, a, _, httpUp := adaptiveFixture()
	s.cfg.Gateway.OpenAIWS.IngressInterTurnIdleTimeoutSeconds = 30
	server, _ := startAdaptiveServer(t, s, a, nil)
	c := dialAdaptive(t, server)
	writeAdaptive(t, c, `{"type":"response.create","model":"gpt-5.1","input":"`+strings.Repeat("x", 65*1024*1024)+`"}`)
	readAdaptiveCompleted(t, c)
	require.Len(t, httpUp.snapshot(), 1)
	require.Greater(t, len(httpUp.snapshot()[0]), 64*1024*1024)
}

func TestAdaptiveRejectsOverlappingCreate(t *testing.T) {
	s, a, ws, _ := adaptiveFixture()
	server, errs := startAdaptiveServer(t, s, a, nil)
	c := dialAdaptive(t, server)
	writeAdaptive(t, c, `{"type":"response.create","model":"gpt-5.1","input":"first"}`)
	_ = requirePassthroughUpstreamWrite(t, ws, 3*time.Second)
	writeAdaptive(t, c, `{"type":"response.create","model":"gpt-5.1","input":"overlap"}`)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _, _ = c.Read(ctx)
	select {
	case err := <-errs:
		require.ErrorContains(t, err, "overlapping")
	case <-time.After(3 * time.Second):
		t.Fatal("overlap did not terminate session")
	}
}

func TestAdaptiveTransportRoutingAndDisabledRouter(t *testing.T) {
	s, a, _, _ := adaptiveFixture()
	require.True(t, s.IsOpenAIWSAdaptive(a))
	require.Equal(t, OpenAIUpstreamTransportResponsesWebsocketV2, s.getOpenAIWSProtocolResolver().Resolve(a).Transport)
	require.True(t, s.isOpenAIAccountTransportCompatible(a, OpenAIUpstreamTransportResponsesWebsocketV2Ingress))
	s.cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = false
	require.False(t, s.IsOpenAIWSAdaptive(a))
}

func TestAdaptivePreflightChecksMixedAndLegacyRoutes(t *testing.T) {
	s, a, _, _ := adaptiveFixture()
	legacy := *a
	legacy.ID = 902
	legacy.Extra = map[string]any{"openai_apikey_responses_websockets_v2_mode": "passthrough"}
	for _, tc := range []struct {
		name     string
		accounts []Account
		want     bool
	}{{"legacy", []Account{legacy}, false}, {"mixed", []Account{legacy, *a}, true}, {"empty", nil, false}} {
		t.Run(tc.name, func(t *testing.T) {
			s.accountRepo = schedulerTestOpenAIAccountRepo{accounts: tc.accounts}
			got, err := s.OpenAIWSAdaptiveRouteAvailable(context.Background(), nil)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

type adaptiveBlockingHTTP struct {
	started   chan struct{}
	cancelled chan struct{}
}

func (h *adaptiveBlockingHTTP) Do(r *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	close(h.started)
	<-r.Context().Done()
	close(h.cancelled)
	return nil, r.Context().Err()
}
func (h *adaptiveBlockingHTTP) DoWithTLS(r *http.Request, p string, id int64, n int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return h.Do(r, p, id, n)
}
func TestAdaptiveBridgeCancelsUpstream(t *testing.T) {
	for _, action := range []string{"disconnect", "overlap"} {
		t.Run(action, func(t *testing.T) {
			s, a, _, _ := adaptiveFixture()
			blocked := &adaptiveBlockingHTTP{started: make(chan struct{}), cancelled: make(chan struct{})}
			s.httpUpstream = blocked
			server, errs := startAdaptiveServer(t, s, a, nil)
			c := dialAdaptive(t, server)
			writeAdaptive(t, c, `{"type":"response.create","model":"gpt-5.1","input":"`+strings.Repeat("x", 2048)+`"}`)
			select {
			case <-blocked.started:
			case <-time.After(3 * time.Second):
				t.Fatal("upstream not started")
			}
			if action == "disconnect" {
				_ = c.CloseNow()
			} else {
				writeAdaptive(t, c, `{"type":"response.create","model":"gpt-5.1","input":"overlap"}`)
			}
			select {
			case <-blocked.cancelled:
			case <-time.After(3 * time.Second):
				t.Fatal("upstream request was not cancelled")
			}
			select {
			case err := <-errs:
				if action == "overlap" {
					require.ErrorContains(t, err, "overlapping")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("bridge did not exit")
			}
		})
	}
}
func TestAdaptiveThresholdBoundary(t *testing.T) {
	payload := `{"type":"response.create","model":"gpt-5.1","input":"small"}`
	for _, offset := range []int{-1, 0, 1} {
		t.Run(fmt.Sprint(offset), func(t *testing.T) {
			s, a, ws, httpUp := adaptiveFixture()
			s.cfg.Gateway.OpenAIWS.HTTPBridgeThresholdBytes = int64(len(payload) + offset)
			server, _ := startAdaptiveServer(t, s, a, nil)
			c := dialAdaptive(t, server)
			writeAdaptive(t, c, payload)
			if offset > 0 {
				respondAdaptiveProbeWS(t, ws)
			}
			readAdaptiveCompleted(t, c)
			if offset > 0 {
				require.Empty(t, httpUp.snapshot())
			} else {
				require.Len(t, httpUp.snapshot(), 1)
			}
		})
	}
}

func TestAdaptiveWarmupDoesNotCommitPassthrough(t *testing.T) {
	s, a, ws, httpUp := adaptiveFixture()
	server, errs := startAdaptiveServer(t, s, a, nil)
	client := dialAdaptive(t, server)
	writeAdaptive(t, client, `{"type":"response.create","model":"gpt-5.1","generate":false,"input":[{"role":"user","content":"warmup context"}]}`)
	sent := requirePassthroughUpstreamWrite(t, ws, 3*time.Second)
	require.Equal(t, gjson.False, gjson.GetBytes(sent, "generate").Type)
	ws.Send(`{"type":"response.completed","response":{"id":"resp_warmup","model":"gpt-5.1","output":[],"usage":{"input_tokens":10,"output_tokens":0}}}`)
	readAdaptiveCompleted(t, client)
	writeAdaptive(t, client, `{"type":"response.create","model":"gpt-5.1","previous_response_id":"resp_warmup","input":[{"role":"user","content":"`+strings.Repeat("x", 2048)+`"}]}`)
	require.Eventually(t, func() bool { return len(httpUp.snapshot()) == 1 }, 3*time.Second, time.Millisecond, "business request must reach HTTP without client reconnect")
	readAdaptiveCompleted(t, client)
	requests := httpUp.snapshot()
	require.Contains(t, string(requests[0]), "warmup context")
	require.False(t, gjson.GetBytes(requests[0], "previous_response_id").Exists())
	require.False(t, gjson.GetBytes(requests[0], "generate").Exists())
	_ = client.CloseNow()
	select {
	case <-errs:
	case <-time.After(3 * time.Second):
		t.Fatal("session did not exit")
	}
}

type adaptiveWarmupDialer struct{ conns chan *stagedPassthroughConn }

func (d *adaptiveWarmupDialer) Dial(ctx context.Context, _ string, _ http.Header, _ string) (openAIWSClientConn, int, http.Header, error) {
	select {
	case c := <-d.conns:
		return c, 101, http.Header{}, nil
	case <-ctx.Done():
		return nil, 0, nil, ctx.Err()
	}
}

func TestAdaptiveWarmupLargeBusinessCompaction(t *testing.T) {
	s, a, ws, httpUp := adaptiveFixture()
	resumed := newStagedPassthroughConn()
	d := &adaptiveWarmupDialer{conns: make(chan *stagedPassthroughConn, 2)}
	d.conns <- ws
	d.conns <- resumed
	s.openaiWSPassthroughDialer = d
	s.cfg.Gateway.OpenAIWS.IngressInterTurnIdleTimeoutSeconds = 30
	s.cfg.Gateway.OpenAIFirstOutputTimeoutSeconds = 30
	var mu sync.Mutex
	var rounds []int
	var transports []bool
	hooks := &OpenAIWSIngressHooks{AfterTurn: func(n int, r *OpenAIForwardResult, e error) {
		if e == nil && r != nil {
			mu.Lock()
			defer mu.Unlock()
			rounds = append(rounds, n)
			transports = append(transports, r.OpenAIWSHTTPBridge)
		}
	}}
	server, errs := startAdaptiveServer(t, s, a, hooks)
	client := dialAdaptive(t, server)
	writeAdaptive(t, client, `{"type":"response.create","model":"gpt-5.1","generate":false,"input":"warmup"}`)
	requirePassthroughUpstreamWrite(t, ws, 3*time.Second)
	ws.Send(`{"type":"response.completed","response":{"id":"resp_warmup","output":[],"usage":{"input_tokens":2,"output_tokens":0}}}`)
	readAdaptiveCompleted(t, client)
	// A restored full request replaces the prewarm context and exceeds the old 64 MiB limit.
	writeAdaptive(t, client, `{"type":"response.create","model":"gpt-5.1","input":"`+strings.Repeat("x", 65*1024*1024)+`"}`)
	readAdaptiveCompleted(t, client)
	require.Len(t, httpUp.snapshot(), 1)
	require.NotContains(t, string(httpUp.snapshot()[0]), "warmup")
	writeAdaptive(t, client, `{"type":"response.create","model":"gpt-5.1","input":[{"type":"compaction","encrypted_content":"short"}]}`)
	requirePassthroughUpstreamWrite(t, resumed, 3*time.Second)
	resumed.Send(`{"type":"response.completed","response":{"id":"resp_resumed","usage":{"input_tokens":5,"output_tokens":1}}}`)
	readAdaptiveCompleted(t, client)
	writeAdaptive(t, client, `{"type":"response.create","previous_response_id":"resp_resumed","input":"next"}`)
	requirePassthroughUpstreamWrite(t, resumed, 3*time.Second)
	resumed.Send(`{"type":"response.completed","response":{"id":"resp_next","usage":{"input_tokens":6,"output_tokens":1}}}`)
	readAdaptiveCompleted(t, client)
	require.Eventually(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(rounds) == 4 }, time.Second, time.Millisecond)
	mu.Lock()
	require.Equal(t, []int{1, 2, 3, 4}, rounds)
	require.Equal(t, []bool{false, true, false, false}, transports)
	mu.Unlock()
	_ = client.CloseNow()
	select {
	case <-errs:
	case <-time.After(3 * time.Second):
		t.Fatal("session did not exit")
	}
}

func TestAdaptiveWarmupFailureDoesNotStartHTTP(t *testing.T) {
	s, a, ws, httpUp := adaptiveFixture()
	server, errs := startAdaptiveServer(t, s, a, nil)
	client := dialAdaptive(t, server)
	writeAdaptive(t, client, `{"type":"response.create","model":"gpt-5.1","generate":false,"input":"warmup"}`)
	requirePassthroughUpstreamWrite(t, ws, 3*time.Second)
	ws.Send(`{"type":"response.failed","response":{"id":"resp_bad","error":{"code":"invalid_request_error","message":"bad request"},"usage":{"input_tokens":2,"output_tokens":0}}}`)
	select {
	case err := <-errs:
		require.Error(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("failed warmup did not terminate")
	}
	require.Empty(t, httpUp.snapshot())
	_ = client.CloseNow()
}

func TestAdaptiveWarmupSmallBusinessAndCancellation(t *testing.T) {
	for _, scenario := range []string{"small", "disconnect", "overlap", "oversized_warmup"} {
		t.Run(scenario, func(t *testing.T) {
			s, a, ws, httpUp := adaptiveFixture()
			next := newStagedPassthroughConn()
			d := &adaptiveWarmupDialer{conns: make(chan *stagedPassthroughConn, 2)}
			d.conns <- ws
			d.conns <- next
			s.openaiWSPassthroughDialer = d
			server, errs := startAdaptiveServer(t, s, a, nil)
			client := dialAdaptive(t, server)
			input := "seed"
			if scenario == "oversized_warmup" {
				input = strings.Repeat("x", 2048)
			}
			writeAdaptive(t, client, `{"type":"response.create","model":"gpt-5.1","generate":false,"input":"`+input+`"}`)
			if scenario != "oversized_warmup" {
				requirePassthroughUpstreamWrite(t, ws, 3*time.Second)
			}
			switch scenario {
			case "small":
				ws.Send(`{"type":"response.completed","response":{"id":"resp_seed","output":[],"usage":{"input_tokens":1,"output_tokens":0}}}`)
				readAdaptiveCompleted(t, client)
				writeAdaptive(t, client, `{"type":"response.create","model":"gpt-5.1","previous_response_id":"resp_seed","input":"business"}`)
				p := requirePassthroughUpstreamWrite(t, ws, 3*time.Second)
				require.NotContains(t, gjson.GetBytes(p, "input").String(), "seed")
				require.Contains(t, string(p), "business")
				require.False(t, gjson.GetBytes(p, "generate").Exists())
				require.Equal(t, "resp_seed", gjson.GetBytes(p, "previous_response_id").String())
				require.Len(t, d.conns, 1, "small business must reuse the warmup socket")
				ws.Send(`{"type":"response.completed","response":{"id":"resp_business","usage":{"input_tokens":2,"output_tokens":1}}}`)
				readAdaptiveCompleted(t, client)
				_ = client.CloseNow()
			case "disconnect":
				_ = client.CloseNow()
			case "overlap":
				writeAdaptive(t, client, `{"type":"response.create","model":"gpt-5.1","input":"overlap"}`)
				readCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				_, _, readErr := client.Read(readCtx)
				cancel()
				require.Error(t, readErr)
			}
			select {
			case err := <-errs:
				if scenario == "overlap" || scenario == "oversized_warmup" {
					require.Error(t, err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("session did not exit")
			}
			require.Empty(t, httpUp.snapshot())
			_ = client.CloseNow()
		})
	}
}

// Repeated generate=false requests remain provisional and share one upstream.
func TestAdaptiveRepeatedWarmupsRetainSocketUntilBusiness(t *testing.T) {
	for _, scenario := range []string{"small", "large", "idle", "disconnect", "failure"} {
		t.Run(scenario, func(t *testing.T) {
			s, a, ws, httpUp := adaptiveFixture()
			d := &adaptiveWarmupDialer{conns: make(chan *stagedPassthroughConn, 1)}
			d.conns <- ws
			s.openaiWSPassthroughDialer = d
			s.cfg.Gateway.OpenAIWS.IngressInterTurnIdleTimeoutSeconds = 1
			var mu sync.Mutex
			var turns []int
			hooks := &OpenAIWSIngressHooks{AfterTurn: func(n int, _ *OpenAIForwardResult, _ error) { mu.Lock(); turns = append(turns, n); mu.Unlock() }}
			server, errs := startAdaptiveServer(t, s, a, hooks)
			client := dialAdaptive(t, server)
			for n := 1; n <= 3; n++ {
				ref := ""
				if n > 1 {
					ref = fmt.Sprintf(`,"previous_response_id":"resp_warm_%d"`, n-1)
				}
				writeAdaptive(t, client, fmt.Sprintf(`{"type":"response.create","model":"gpt-5.1","generate":false,"input":"seed%d"%s}`, n, ref))
				p := requirePassthroughUpstreamWrite(t, ws, 3*time.Second)
				require.Equal(t, fmt.Sprintf("seed%d", n), gjson.GetBytes(p, "input").String())
				if n > 1 {
					require.Equal(t, fmt.Sprintf("resp_warm_%d", n-1), gjson.GetBytes(p, "previous_response_id").String())
				}
				ws.Send(fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_warm_%d","output":[],"usage":{"input_tokens":1,"output_tokens":0}}}`, n))
				readAdaptiveCompleted(t, client)
				select {
				case <-ws.closed:
					t.Fatal("warmup closed the reusable socket")
				default:
				}
			}
			switch scenario {
			case "small", "large", "failure":
				input := "business"
				if scenario == "large" {
					input = strings.Repeat("x", 2048)
				}
				writeAdaptive(t, client, `{"type":"response.create","model":"gpt-5.1","previous_response_id":"resp_warm_3","input":"`+input+`"}`)
				if scenario == "large" {
					readAdaptiveCompleted(t, client)
					require.Len(t, httpUp.snapshot(), 1)
					for n := 1; n <= 3; n++ {
						require.Contains(t, string(httpUp.snapshot()[0]), fmt.Sprintf("seed%d", n))
					}
					select {
					case <-ws.closed:
					default:
						t.Fatal("HTTP selection must release warmup socket")
					}
				} else {
					p := requirePassthroughUpstreamWrite(t, ws, 3*time.Second)
					require.Equal(t, "resp_warm_3", gjson.GetBytes(p, "previous_response_id").String())
					require.Equal(t, "business", gjson.GetBytes(p, "input").String())
					if scenario == "failure" {
						_ = ws.Close()
					} else {
						ws.Send(`{"type":"response.completed","response":{"id":"resp_business","usage":{"input_tokens":4,"output_tokens":1}}}`)
						readAdaptiveCompleted(t, client)
					}
				}
				_ = client.CloseNow()
			case "disconnect":
				_ = client.CloseNow()
			case "idle":
				ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
				_, _, err := client.Read(ctx)
				cancel()
				require.Error(t, err)
			}
			select {
			case <-errs:
			case <-time.After(4 * time.Second):
				t.Fatal("session did not exit")
			}
			select {
			case <-ws.closed:
			default:
				t.Fatal("session exit leaked warmup socket")
			}
			if scenario != "large" {
				require.Empty(t, httpUp.snapshot())
			}
			mu.Lock()
			defer mu.Unlock()
			require.GreaterOrEqual(t, len(turns), 3)
			for i, n := range turns {
				require.Equal(t, i+1, n)
			}
		})
	}
}

// Exercise coder/websocket as well as the staged transport: cancelling a live
// Read can close a real socket, so successful SingleTurn must leave no read behind.
type adaptiveLocalDialer struct {
	url   string
	calls atomic.Int32
}

func (d *adaptiveLocalDialer) Dial(ctx context.Context, _ string, h http.Header, _ string) (openAIWSClientConn, int, http.Header, error) {
	d.calls.Add(1)
	return newDefaultOpenAIWSClientDialer().Dial(ctx, d.url, h, "")
}

func TestAdaptiveWarmupReusesRealWebSocket(t *testing.T) {
	received := make(chan []byte, 8)
	upstreamDone := make(chan struct{}, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow(); upstreamDone <- struct{}{} }()
		for n := 1; ; n++ {
			_, p, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			received <- p
			output := 0
			if gjson.GetBytes(p, "generate").Type != gjson.False {
				output = 1
			}
			response := fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_real_%d","output":[],"usage":{"input_tokens":1,"output_tokens":%d}}}`, n, output)
			if conn.Write(r.Context(), coderws.MessageText, []byte(response)) != nil {
				return
			}
		}
	}))
	defer upstream.Close()
	s, a, _, httpUp := adaptiveFixture()
	d := &adaptiveLocalDialer{url: "ws" + strings.TrimPrefix(upstream.URL, "http")}
	s.openaiWSPassthroughDialer = d
	server, errs := startAdaptiveServer(t, s, a, nil)
	client := dialAdaptive(t, server)
	for n := 1; n <= 4; n++ {
		extra := ""
		if n <= 2 {
			extra += `,"generate":false`
		}
		if n > 1 {
			extra += fmt.Sprintf(`,"previous_response_id":"resp_real_%d"`, n-1)
		}
		writeAdaptive(t, client, fmt.Sprintf(`{"type":"response.create","model":"gpt-5.1","input":"turn%d"%s}`, n, extra))
		readAdaptiveCompleted(t, client)
		select {
		case p := <-received:
			require.Equal(t, fmt.Sprintf("turn%d", n), gjson.GetBytes(p, "input").String())
			if n > 1 {
				require.Equal(t, fmt.Sprintf("resp_real_%d", n-1), gjson.GetBytes(p, "previous_response_id").String())
			}
		case <-time.After(3 * time.Second):
			t.Fatal("upstream did not receive request")
		}
	}
	require.EqualValues(t, 1, d.calls.Load())
	require.Empty(t, httpUp.snapshot())
	_ = client.CloseNow()
	select {
	case <-errs:
	case <-time.After(4 * time.Second):
		t.Fatal("session did not exit")
	}
	select {
	case <-upstreamDone:
	case <-time.After(4 * time.Second):
		t.Fatal("upstream connection leaked")
	}
}
