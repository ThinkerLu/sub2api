package service

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// 启用组在“全程无 WS 腿的 HTTP bridge”上不得被 WSv2 守卫误拒：连接确实改走
// HTTP 上游（httpUpstreamRecorder 收到请求），且没有向上游 WS 拨号或写帧。
//
// 两种纯 bridge 形态都覆盖：
//   - 账号显式模式 http_bridge（解析出的上游传输已经是 HTTP）；
//   - ctx_pool 直连模式下由大小阈值改走 bridge（上游传输本可以是 WSv2，
//     但这条连接全程只走 HTTP）。
func TestCodexV2WSIngressPureHTTPBridgeIsAllowedForEnabledGroup(t *testing.T) {
	policy, err := NewCodexV2GroupPolicyFromBytes([]byte("version: 1\ngroups:\n  \"77\":\n    enabled: true\n"))
	require.NoError(t, err)
	restore := SetCurrentCodexV2GroupPolicyForTest(policy)
	t.Cleanup(restore)

	tests := []struct {
		name      string
		configure func(setup *codexV2WSTestSetup)
	}{
		{
			name: "explicit_http_bridge_mode",
			configure: func(setup *codexV2WSTestSetup) {
				setup.service.cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
				setup.service.cfg.Gateway.OpenAIWS.IngressModeDefault = OpenAIWSIngressModeCtxPool
				setup.service.cfg.Gateway.OpenAIWS.HTTPBridgeEnabled = true
				setup.account.Extra["openai_apikey_responses_websockets_v2_mode"] = OpenAIWSIngressModeHTTPBridge
			},
		},
		{
			name: "size_triggered_bridge_from_ctx_pool",
			configure: func(setup *codexV2WSTestSetup) {
				setup.service.cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = false
				setup.service.cfg.Gateway.OpenAIWS.HTTPBridgeEnabled = true
				setup.service.cfg.Gateway.OpenAIWS.HTTPBridgeThresholdBytes = 1
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			setup := newCodexV2WSTestSetup(t)
			tt.configure(setup)
			setup.httpUpstream.resp = &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body: io.NopCloser(strings.NewReader(
					"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_bridge\",\"model\":\"gpt-5.1\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")),
			}

			// 前提断言：即使 socket 部分在受限环境被跳过，也要钉住“这条连接本该
			// 全程走 HTTP bridge”的判定。
			if tt.name == "explicit_http_bridge_mode" {
				require.Equal(t, OpenAIWSIngressModeHTTPBridge,
					setup.account.ResolveOpenAIResponsesWebSocketV2Mode(setup.service.cfg.Gateway.OpenAIWS.IngressModeDefault))
			} else {
				require.Equal(t, OpenAIUpstreamTransportResponsesWebsocketV2,
					setup.service.getOpenAIWSProtocolResolver().Resolve(&setup.account).Transport,
					"premise: this case starts from a WSv2-capable ctx_pool route")
				require.True(t, setup.service.shouldBridgeOpenAIWSHTTP(&setup.account, 64, "", "gpt-5.1"),
					"premise: the size threshold must force this connection onto the HTTP bridge")
			}

			ginCtx := newCodexV2WSGinContext(t, 77)
			ingressErr := runCodexV2WSIngressTurn(t, setup, ginCtx)
			require.False(t, isCodexV2PolicyGuardRejection(ingressErr),
				"a pure HTTP bridge connection must not be rejected by the WSv2 guard, got %v", ingressErr)

			require.NotEmpty(t, setup.httpUpstream.bodies,
				"the enabled group must reach the HTTP upstream through the bridge")
			require.Equal(t, "true", gjson.GetBytes(setup.httpUpstream.bodies[0], "stream").String(),
				"the bridge must send a streaming HTTP request upstream")
			require.Zero(t, setup.dialer.DialCount(), "a pure HTTP bridge must not dial an upstream websocket")
			require.Empty(t, setup.captureConn.writes, "a pure HTTP bridge must not write any upstream websocket frame")
		})
	}
}

// isCodexV2PolicyGuardRejection 只识别“被 Codex V2 的 WSv2 守卫拒绝”这一类错误，
// 便于断言纯 HTTP bridge 没有被误拒（其它上游/客户端错误不影响该断言）。
func isCodexV2PolicyGuardRejection(err error) bool {
	if err == nil {
		return false
	}
	var closeErr *OpenAIWSClientCloseError
	if !errors.As(err, &closeErr) {
		return false
	}
	return strings.Contains(closeErr.Reason(), codexV2WSv2UnsupportedMessage)
}
