package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestResponsesWSProfileRequiresExplicitProviderConfiguration(t *testing.T) {
	s, _, _, _ := adaptiveFixture()
	s.cfg.Gateway.OpenAIWS.DualLegEnabled = true
	a := &Account{Platform: PlatformDeepseek, Type: AccountTypeAPIKey, Concurrency: 1, Credentials: map[string]any{"base_url": "https://provider.example", "api_protocol": "responses"}, Extra: map[string]any{}}
	require.Equal(t, OpenAIWSIngressModeOff, a.ResolveOpenAIResponsesWebSocketV2Mode("ctx_pool"))
	a.Extra["responses_ws_mode"] = "adaptive"
	require.Equal(t, OpenAIUpstreamTransportHTTPSSE, s.getOpenAIWSProtocolResolver().Resolve(a).Transport)
	_, err := s.buildOpenAIResponsesWSURL(a)
	require.Error(t, err)
	a.Extra["responses_ws_profile"] = ResponsesWSProfileOpenAIV2
	a.Extra["responses_ws_url"] = "wss://provider.example/custom-responses"
	require.Equal(t, OpenAIUpstreamTransportResponsesWebsocketV2, s.getOpenAIWSProtocolResolver().Resolve(a).Transport)
	endpoint, err := s.buildOpenAIResponsesWSURL(a)
	require.NoError(t, err)
	require.Equal(t, "wss://provider.example/custom-responses", endpoint)
	headers, _, err := s.buildOpenAIWSHeaders(context.Background(), nil, a, "synthetic-token", s.getOpenAIWSProtocolResolver().Resolve(a), false, "", "", "", "deepseek-v4-pro", "")
	require.NoError(t, err)
	require.Equal(t, "Bearer synthetic-token", headers.Get("Authorization"))
	for _, bad := range []string{"wss://user:password@provider.example/ws", "wss://provider.example/ws?token=x", "https://provider.example/ws", "wss://provider.example/ws#fragment"} {
		a.Extra["responses_ws_url"] = bad
		require.Empty(t, a.ResponsesWSV2Profile())
	}
}

func TestDualLegNativeProviderWithoutWSProfileUsesHTTP(t *testing.T) {
	s, a, ws, httpUp := adaptiveFixture()
	s.cfg.Gateway.OpenAIWS.DualLegEnabled = true
	a.Platform = PlatformDeepseek
	a.Credentials["api_protocol"] = "responses"
	server, _ := startAdaptiveServer(t, s, a, nil)
	client := dialAdaptive(t, server)
	writeAdaptive(t, client, `{"type":"response.create","model":"deepseek-v4-pro","input":[{"role":"user","content":"hello"},{"type":"reasoning","content":[{"type":"reasoning_text","text":"keep native thinking"}]}]}`)
	readAdaptiveCompleted(t, client)
	writeAdaptive(t, client, `{"type":"response.create","model":"deepseek-v4-pro","previous_response_id":"resp_http_1","input":[{"role":"user","content":"next"}]}`)
	readAdaptiveCompleted(t, client)
	requests := httpUp.snapshot()
	require.Len(t, requests, 2)
	require.Empty(t, ws.writes)
	require.Contains(t, string(requests[1]), "keep native thinking")
	require.Empty(t, gjson.GetBytes(requests[1], "previous_response_id").String())
}

func TestDualLegNativeProviderExplicitProfileUsesWSWithoutGPTThreshold(t *testing.T) {
	s, a, ws, httpUp := adaptiveFixture()
	s.cfg.Gateway.OpenAIWS.DualLegEnabled = true
	s.cfg.Gateway.OpenAIWS.HTTPBridgeThresholdBytes = 1
	a.Platform = PlatformDeepseek
	a.Credentials["api_protocol"] = "responses"
	a.Credentials["base_url"] = "http://127.0.0.1:1"
	a.Extra["responses_ws_mode"] = "adaptive"
	a.Extra["responses_ws_profile"] = ResponsesWSProfileOpenAIV2
	a.Extra["responses_ws_url"] = "ws://127.0.0.1:1/explicit"
	server, _ := startAdaptiveServer(t, s, a, nil)
	client := dialAdaptive(t, server)
	writeAdaptive(t, client, `{"type":"response.create","model":"deepseek-v4-pro","input":[{"role":"user","content":"hello"}]}`)
	body := requirePassthroughUpstreamWrite(t, ws, time.Second)
	require.Equal(t, "deepseek-v4-pro", gjson.GetBytes(body, "model").String())
	ws.Send(`{"type":"response.completed","response":{"id":"resp_native","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`)
	readAdaptiveCompleted(t, client)
	require.Empty(t, httpUp.snapshot())
}
