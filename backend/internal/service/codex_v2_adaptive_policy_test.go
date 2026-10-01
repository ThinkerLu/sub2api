package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// codexV2AdaptiveFixture 复用既有 adaptive 夹具，但把上游 WS 换成一个记录帧的
// 本地服务，并把拨号器换成带计数器的实现，便于断言“零拨号、零写帧”。
type codexV2AdaptiveFixture struct {
	service      *OpenAIGatewayService
	account      *Account
	dialer       *adaptiveLocalDialer
	httpUpstream *adaptiveHTTPUpstream
	received     chan []byte
}

func newCodexV2AdaptiveFixture(t *testing.T) *codexV2AdaptiveFixture {
	t.Helper()
	received := make(chan []byte, 8)
	upstream := newCodexV2WSTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		for {
			_, payload, readErr := conn.Read(r.Context())
			if readErr != nil {
				return
			}
			select {
			case received <- append([]byte(nil), payload...):
			default:
			}
			_ = conn.Write(r.Context(), coderws.MessageText,
				[]byte(`{"type":"response.completed","response":{"id":"resp_ws","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`))
		}
	}))
	t.Cleanup(upstream.Close)

	svc, account, _, httpUpstream := adaptiveFixture()
	dialer := &adaptiveLocalDialer{url: "ws" + strings.TrimPrefix(upstream.URL, "http")}
	svc.openaiWSPassthroughDialer = dialer
	require.True(t, svc.IsOpenAIWSAdaptive(account),
		"fixture account must resolve to adaptive, otherwise the WS-leg guard is meaningless")
	require.Equal(t, OpenAIUpstreamTransportResponsesWebsocketV2,
		svc.getOpenAIWSProtocolResolver().Resolve(account).Transport,
		"fixture account must have a native WSv2 leg available")
	return &codexV2AdaptiveFixture{
		service:      svc,
		account:      account,
		dialer:       dialer,
		httpUpstream: httpUpstream,
		received:     received,
	}
}

// 连接级判定必须离线可测：adaptive 只要具备原生 WSv2 能力就不能被当成纯 HTTP
// 放行，而明确只走 HTTP bridge 的连接保持可用。
func TestCodexV2WSv2HTTPBridgeOnly(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		bridge      bool
		ingressMode string
		transport   OpenAIUpstreamTransport
		want        bool
	}{
		{name: "adaptive_with_ws_leg", bridge: true, ingressMode: OpenAIWSIngressModeAdaptive, transport: OpenAIUpstreamTransportResponsesWebsocketV2, want: false},
		{name: "adaptive_without_ws_leg", bridge: true, ingressMode: OpenAIWSIngressModeAdaptive, transport: OpenAIUpstreamTransportHTTPSSE, want: true},
		{name: "explicit_http_bridge_mode", bridge: true, ingressMode: OpenAIWSIngressModeHTTPBridge, transport: OpenAIUpstreamTransportResponsesWebsocketV2, want: true},
		{name: "explicit_http_bridge_mode_with_http_transport", bridge: true, ingressMode: OpenAIWSIngressModeHTTPBridge, transport: OpenAIUpstreamTransportHTTPSSE, want: true},
		{name: "passthrough_size_bridged", bridge: true, ingressMode: OpenAIWSIngressModePassthrough, transport: OpenAIUpstreamTransportResponsesWebsocketV2, want: true},
		{name: "ctx_pool_size_bridged", bridge: true, ingressMode: OpenAIWSIngressModeCtxPool, transport: OpenAIUpstreamTransportResponsesWebsocketV2, want: true},
		{name: "ctx_pool_direct", bridge: false, ingressMode: OpenAIWSIngressModeCtxPool, transport: OpenAIUpstreamTransportResponsesWebsocketV2, want: false},
		{name: "passthrough_direct", bridge: false, ingressMode: OpenAIWSIngressModePassthrough, transport: OpenAIUpstreamTransportResponsesWebsocketV2, want: false},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, codexV2WSv2HTTPBridgeOnly(tt.bridge, tt.ingressMode, tt.transport))
		})
	}
}

// startCodexV2AdaptiveServer 与既有 startAdaptiveServer 相同，但补上带 GroupID 的
// api_key 上下文，让分组策略在真实 ingress 入口生效。
func startCodexV2AdaptiveServer(t *testing.T, svc *OpenAIGatewayService, account *Account, groupID int64) (*httptest.Server, <-chan error) {
	t.Helper()
	errs := make(chan error, 1)
	server := newCodexV2WSTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, &coderws.AcceptOptions{CompressionMode: coderws.CompressionContextTakeover})
		if err != nil {
			errs <- err
			return
		}
		defer conn.CloseNow()
		conn.SetReadLimit(ResolveOpenAIWSClientReadLimitBytes(svc.cfg))
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		_, first, readErr := ReadOpenAIWSClientMessage(ctx, conn, 0, coderws.StatusPolicyViolation, "")
		if readErr != nil {
			errs <- readErr
			return
		}
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = r.Clone(ctx)
		c.Request.Header = r.Header.Clone()
		id := groupID
		c.Set("api_key", &APIKey{ID: 77, GroupID: &id})
		errs <- svc.ProxyResponsesWebSocketFromClient(ctx, c, conn, account, "sk-test", first, nil)
	}))
	t.Cleanup(server.Close)
	return server, errs
}

func TestCodexV2AdaptiveRewritesEnabledGroupOnBothLegs(t *testing.T) {
	gin.SetMode(gin.TestMode)

	policy, err := NewCodexV2GroupPolicyFromBytes([]byte("version: 1\ngroups:\n  \"77\":\n    enabled: true\n"))
	require.NoError(t, err)
	restore := SetCurrentCodexV2GroupPolicyForTest(policy)
	t.Cleanup(restore)

	tests := []struct {
		name    string
		payload string
	}{
		{
			name:    "warmup_ws_leg",
			payload: `{"type":"response.create","model":"gpt-5.1","generate":false,"input":"warmup"}`,
		},
		{
			name:    "generating_first_frame_ws_leg",
			payload: `{"type":"response.create","model":"gpt-5.1","input":"small"}`,
		},
		{
			// 首轮大到走 HTTP bridge、后续轮才切入 WS 的形态：因为守卫在连接级、
			// 且发生在进入 adaptive 循环之前，这条连接同样不得产生任何上游联系。
			name:    "http_first_then_later_ws_turn",
			payload: `{"type":"response.create","model":"gpt-5.1","input":"` + strings.Repeat("x", 4096) + `"}`,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			fx := newCodexV2AdaptiveFixture(t)
			server, _ := startCodexV2AdaptiveServer(t, fx.service, fx.account, 77)
			client := dialAdaptive(t, server)
			tools := gjson.Get(`{`+codexV2BridgeToolsDeclaration+`}`, "tools").Raw
			payload, err := sjson.SetRaw(tt.payload, "tools", tools)
			require.NoError(t, err)
			writeAdaptive(t, client, payload)
			require.Eventually(t, func() bool { return len(fx.received) > 0 || len(fx.httpUpstream.snapshot()) > 0 }, 5*time.Second, 10*time.Millisecond)
			var frame []byte
			select {
			case frame = <-fx.received:
			default:
				frame = fx.httpUpstream.snapshot()[0]
			}
			require.Contains(t, string(frame), "sub2api_collaboration")
			require.NotContains(t, string(frame), `"encrypted"`)
			_ = client.CloseNow()
		})
	}
}

func TestCodexV2AdaptiveKeepsDisabledGroupBehaviour(t *testing.T) {
	gin.SetMode(gin.TestMode)

	policy, err := NewCodexV2GroupPolicyFromBytes([]byte("version: 1\ngroups:\n  \"77\":\n    enabled: true\n"))
	require.NoError(t, err)
	restore := SetCurrentCodexV2GroupPolicyForTest(policy)
	t.Cleanup(restore)

	fx := newCodexV2AdaptiveFixture(t)
	server, _ := startCodexV2AdaptiveServer(t, fx.service, fx.account, 88)
	client := dialAdaptive(t, server)
	writeAdaptive(t, client, `{"type":"response.create","model":"gpt-5.1","input":"small"}`)

	select {
	case frame := <-fx.received:
		require.Contains(t, string(frame), "small", "disabled group must keep sending the original frame upstream")
	case <-time.After(5 * time.Second):
		t.Fatal("disabled group did not reach the upstream websocket leg")
	}
	require.EqualValues(t, 1, fx.dialer.calls.Load(), "disabled group must keep dialing the upstream websocket")
	require.Empty(t, fx.httpUpstream.snapshot(), "small adaptive request stays on the websocket leg")
	_ = client.CloseNow()
}
