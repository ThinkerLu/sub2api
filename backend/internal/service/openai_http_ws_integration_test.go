package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Exercise a real HTTP client and a real upstream WS, not only the protocol resolver.
func TestHTTPIngressIndependentWebSocketUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, model := range []string{"deepseek-v4-pro", "gpt-5.1"} {
		for _, platform := range []string{PlatformOpenAI, PlatformDeepseek} {
			if platform != PlatformOpenAI && model == "gpt-5.1" {
				continue
			}
			for _, closeAfterWrite := range []bool{false, true} {
				for _, stream := range []bool{false, true} {
					for _, passthrough := range []bool{false, true} {
						t.Run(fmt.Sprintf("%s/%s/stream=%v/passthrough=%v/close=%v", platform, model, stream, passthrough, closeAfterWrite), func(t *testing.T) {
							var writes atomic.Int32
							frames := make(chan map[string]any, 4)
							up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
								conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
								if err != nil {
									return
								}
								defer conn.Close()
								for {
									var frame map[string]any
									if conn.ReadJSON(&frame) != nil {
										return
									}
									writes.Add(1)
									frames <- frame
									if closeAfterWrite {
										return
									}
									if conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_http_ws","model":"deepseek-v4-pro","status":"completed","output":[],"usage":{"input_tokens":3,"output_tokens":2}}}`)) != nil {
										return
									}
								}
							}))
							defer up.Close()
							cfg := &config.Config{}
							cfg.Security.URLAllowlist.AllowInsecureHTTP = true
							cfg.Gateway.OpenAIWS.Enabled = true
							cfg.Gateway.OpenAIWS.APIKeyEnabled = true
							cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
							cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
							cfg.Gateway.OpenAIWS.IngressModeDefault = OpenAIWSIngressModeAdaptive
							cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
							cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 3
							cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 3
							cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 3
							if model == "deepseek-v4-pro" {
								cfg.Gateway.OpenAIWS.HTTPBridgeThresholdBytes = 1
							}
							httpUpstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"usage":{}}`))}}
							svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: httpUpstream, cache: &stubGatewayCache{}, toolCorrector: NewCodexToolCorrector()}
							account := &Account{ID: 812, Platform: platform, Type: AccountTypeAPIKey, Concurrency: 1,
								Credentials: map[string]any{"api_key": "synthetic", "base_url": up.URL},
								Extra:       map[string]any{"openai_apikey_responses_websockets_v2_mode": "adaptive", "openai_passthrough": passthrough}}
							if platform != PlatformOpenAI {
								account.Credentials["api_protocol"] = "responses"
								account.Extra["responses_ws_mode"] = "adaptive"
								account.Extra["responses_ws_profile"] = ResponsesWSProfileOpenAIV2
								account.Extra["responses_ws_url"] = "ws" + strings.TrimPrefix(up.URL, "http") + "/v1/responses"
							}
							results := make(chan *OpenAIForwardResult, 1)
							errors := make(chan error, 1)
							router := gin.New()
							router.POST("/v1/responses", func(c *gin.Context) {
								SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
								body, _ := io.ReadAll(c.Request.Body)
								result, err := svc.Forward(c.Request.Context(), c, account, body)
								results <- result
								errors <- err
							})
							gateway := httptest.NewServer(router)
							defer gateway.Close()
							defer func() {
								if svc.openaiWSPool != nil {
									svc.openaiWSPool.Close()
								}
							}()
							body := fmt.Sprintf(`{"model":%q,"stream":%v,"input":[{"type":"reasoning","content":[{"type":"reasoning_text","text":"synthetic thinking"}]},{"role":"user","content":"synthetic"}]}`, model, stream)
							ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
							defer cancel()
							req, err := http.NewRequestWithContext(ctx, http.MethodPost, gateway.URL+"/v1/responses", strings.NewReader(body))
							require.NoError(t, err)
							resp, err := http.DefaultClient.Do(req)
							require.NoError(t, err)
							defer resp.Body.Close()
							responseBody, err := io.ReadAll(resp.Body)
							require.NoError(t, err)
							forwardErr := <-errors
							result := <-results
							if closeAfterWrite {
								require.Error(t, forwardErr)
							require.Equal(t, http.StatusBadRequest, resp.StatusCode)
							require.Contains(t, resp.Header.Get("Content-Type"), "application/json")
								require.EqualValues(t, 1, writes.Load(), "unknown execution must not be replayed")
								require.Nil(t, httpUpstream.lastReq, "unknown execution must not fall back to HTTP")
								return
							}
							require.NoError(t, forwardErr)
							require.NotNil(t, result)
							require.True(t, result.OpenAIWSMode, "HTTP ingress must not override an eligible upstream WS")
							require.EqualValues(t, 1, writes.Load())
							require.Nil(t, httpUpstream.lastReq)
							frame := <-frames
							require.Equal(t, "response.create", frame["type"])
							encodedFrame, err := json.Marshal(frame)
							require.NoError(t, err)
							if model == "deepseek-v4-pro" {
								require.Contains(t, string(encodedFrame), "synthetic thinking")
							} else {
								require.NotContains(t, string(encodedFrame), "synthetic thinking")
							}
							logRepo := &openAIRecordUsageLogRepoStub{inserted: true}
							usageService := newOpenAIRecordUsageServiceForTest(logRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)
							require.NoError(t, usageService.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
								IngressTransport: UsageIngressTransportHTTP, Result: result,
								APIKey: &APIKey{ID: 20, UserID: 21, Group: &Group{RateMultiplier: 1}}, User: &User{ID: 21}, Account: account,
							}))
							require.NotNil(t, logRepo.lastLog)
							require.EqualValues(t, UsageIngressTransportHTTP, logRepo.lastLog.IngressTransport)
							require.EqualValues(t, UsageUpstreamTransportWebSocket, logRepo.lastLog.UpstreamTransport)
							if stream {
								require.Contains(t, resp.Header.Get("Content-Type"), "text/event-stream")
								require.Contains(t, string(responseBody), "response.completed")
							} else {
								var response map[string]any
								require.NoError(t, json.Unmarshal(responseBody, &response))
								require.Equal(t, "resp_http_ws", response["id"])
							}
						})
					}
				}
			}
		}
	}

}

func TestHTTPIngressHonorsHTTPRouteConstraints(t *testing.T) {
	for _, tc := range []struct {
		name, mode, path, model string
		force, disabled         bool
		threshold               int64
	}{
		{name: "off", mode: OpenAIWSIngressModeOff},
		{name: "http_bridge", mode: OpenAIWSIngressModeHTTPBridge},
		{name: "force_http", force: true},
		{name: "global_disabled", disabled: true},
		{name: "compact", path: "/v1/responses/compact"},
		{name: "mapped_gpt_threshold", model: "alias", threshold: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wsGuard := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "unexpected WS attempt", 400) }))
			defer wsGuard.Close()
			cfg := &config.Config{}
			cfg.Security.URLAllowlist.AllowInsecureHTTP = true
			cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 1
			cfg.Gateway.OpenAIWS.Enabled = !tc.disabled
			cfg.Gateway.OpenAIWS.APIKeyEnabled = true
			cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
			cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
			cfg.Gateway.OpenAIWS.IngressModeDefault = OpenAIWSIngressModeAdaptive
			if tc.mode != "" {
				cfg.Gateway.OpenAIWS.IngressModeDefault = tc.mode
			}
			cfg.Gateway.OpenAIWS.HTTPBridgeThresholdBytes = tc.threshold
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"id":"resp_http","usage":{"input_tokens":1,"output_tokens":1}}`))}}
			svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream}
			account := &Account{ID: 815, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1,
				Credentials: map[string]any{"api_key": "synthetic", "base_url": wsGuard.URL, "model_mapping": map[string]any{"alias": "gpt-5.1"}},
				Extra:       map[string]any{"openai_apikey_responses_websockets_v2_mode": cfg.Gateway.OpenAIWS.IngressModeDefault, "openai_ws_force_http": tc.force}}
			defer func() {
				if svc.openaiWSPool != nil {
					svc.openaiWSPool.Close()
				}
			}()
			path := tc.path
			if path == "" {
				path = "/v1/responses"
			}
			model := tc.model
			if model == "" {
				model = "deepseek-v4-pro"
			}
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, path, nil)
			SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
			body := []byte(fmt.Sprintf(`{"model":%q,"stream":false,"input":"synthetic"}`, model))
			result, err := svc.Forward(context.Background(), c, account, body)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.False(t, result.OpenAIWSMode)
			require.NotNil(t, upstream.lastReq)
			if tc.threshold > 0 {
				require.Equal(t, "gpt-5.1", gjson.GetBytes(upstream.lastBody, "model").String())
			}
		})
	}
}

func TestHTTPIngressFrameThresholdBeforeUpstreamSubmission(t *testing.T) {
	for _, connectionLocal := range []bool{false, true} {
		t.Run(fmt.Sprint(connectionLocal), func(t *testing.T) {
			var dials atomic.Int32
			guard := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { dials.Add(1); http.Error(w, "unexpected dial", 400) }))
			defer guard.Close()
			cfg := &config.Config{}
			cfg.Security.URLAllowlist.AllowInsecureHTTP = true
			cfg.Gateway.OpenAIWS.Enabled = true
			cfg.Gateway.OpenAIWS.APIKeyEnabled = true
			cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
			cfg.Gateway.OpenAIWS.HTTPBridgeThresholdBytes = 4096
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"id":"resp_threshold","usage":{"input_tokens":1,"output_tokens":1}}`))}}
			svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream}
			account := &Account{ID: 818, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1,
				Credentials: map[string]any{"api_key": "synthetic", "base_url": guard.URL}, Extra: map[string]any{"responses_websockets_v2_enabled": true}}
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			c.Request.Header.Set(openAIWSTurnMetadataHeader, strings.Repeat("x", 8192))
			SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
			body := []byte(`{"model":"gpt-5.1","stream":false,"input":"synthetic"}`)
			if connectionLocal {
				body = []byte(`{"model":"gpt-5.1","stream":false,"store":false,"previous_response_id":"resp_local","input":"delta"}`)
			}
			require.Less(t, len(body), 4096)
			result, err := svc.Forward(context.Background(), c, account, body)
			require.Zero(t, dials.Load(), "wire-size policy must run before acquiring an upstream WS")
			if connectionLocal {
				require.Error(t, err)
				require.Equal(t, http.StatusBadRequest, rec.Code)
				require.Equal(t, "full_context_required", gjson.GetBytes(rec.Body.Bytes(), "error.code").String())
				require.Nil(t, upstream.lastReq, "connection-local continuation cannot move to HTTP")
			} else {
				require.NoError(t, err)
				require.NotNil(t, result)
				require.False(t, result.OpenAIWSMode)
				require.NotNil(t, upstream.lastReq)
				require.Equal(t, "gpt_payload_threshold", c.GetString("openai_ws_transport_reason"))
			}
		})
	}
}

type httpWSFailedWriteConn struct {
	*stagedPassthroughConn
	attempts atomic.Int32
}

func (c *httpWSFailedWriteConn) WriteJSON(context.Context, any) error {
	c.attempts.Add(1)
	return io.ErrClosedPipe
}

func TestHTTPIngressWriteFailureDoesNotReplay(t *testing.T) {
	svc, account, _, _ := adaptiveFixture()
	upstream := &httpUpstreamRecorder{}
	svc.httpUpstream = upstream
	conn := &httpWSFailedWriteConn{stagedPassthroughConn: newStagedPassthroughConn()}
	pool := newOpenAIWSConnPool(svc.cfg)
	pool.setClientDialerForTest(&openAIWSSingleConnDialer{conn: conn})
	svc.openaiWSPool = pool
	defer pool.Close()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
	_, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-5.1","stream":true,"input":"synthetic"}`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "execution unknown")
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.EqualValues(t, 1, conn.attempts.Load())
	require.Nil(t, upstream.lastReq)
}
