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
