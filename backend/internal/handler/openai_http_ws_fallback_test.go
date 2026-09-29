package handler

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestHTTPWSHandlerHandshakeFallbackAndNextRequestUpgrade(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			var ready atomic.Bool
			var handshakes, posts, frames atomic.Int32
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					posts.Add(1)
					body, err := io.ReadAll(r.Body)
					if err != nil {
						return
					}
					if gjson.GetBytes(body, "stream").Bool() {
						w.Header().Set("Content-Type", "text/event-stream")
						fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_fallback_http\",\"model\":\"gpt-5.1\",\"output\":[],\"usage\":{\"input_tokens\":3,\"output_tokens\":2}}}\n\n")
					} else {
						w.Header().Set("Content-Type", "application/json")
						fmt.Fprint(w, `{"id":"resp_fallback_http","model":"gpt-5.1","output":[],"usage":{"input_tokens":3,"output_tokens":2}}`)
					}
					return
				}
				handshakes.Add(1)
				if !ready.Load() {
					http.Error(w, "WS temporarily unavailable", http.StatusUpgradeRequired)
					return
				}
				conn, err := coderws.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer conn.CloseNow()
				_, _, err = conn.Read(r.Context())
				if err != nil {
					return
				}
				frames.Add(1)
				_ = conn.Write(r.Context(), coderws.MessageText, []byte(`{"type":"response.completed","response":{"id":"resp_recovered_ws","model":"gpt-5.1","status":"completed","output":[],"usage":{"input_tokens":3,"output_tokens":2}}}`))
			}))
			defer up.Close()
			h := newOpenAIWSPassthroughHandlerHarness(t, up.URL, func(cfg *config.Config, a *service.Account) {
				a.Extra["openai_apikey_responses_websockets_v2_mode"] = service.OpenAIWSIngressModeAdaptive
				a.Extra["openai_passthrough"] = true
			})
			defer h.gatewayService.CloseOpenAIWSPool()
			_ = h.clientConn.CloseNow()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			for turn, expected := range []int16{service.UsageUpstreamTransportHTTP, service.UsageUpstreamTransportWebSocket} {
				ready.Store(turn == 1)
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.serverURL+"/openai/v1/responses", strings.NewReader(fmt.Sprintf(`{"model":"gpt-5.1","stream":%v,"input":[{"role":"user","content":"full synthetic context"}]}`, stream)))
				require.NoError(t, err)
				req.Header.Set("Content-Type", "application/json")
				resp, err := http.DefaultClient.Do(req)
				require.NoError(t, err)
				body, err := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				require.NoError(t, err)
				require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
				select {
				case usage := <-h.usageRepo.created:
					require.Equal(t, service.UsageIngressTransportHTTP, usage.IngressTransport)
					require.Equal(t, expected, usage.UpstreamTransport)
					require.Equal(t, h.accountRepo.account.ID, usage.AccountID)
				case <-ctx.Done():
					t.Fatal("missing usage")
				}
			}
			require.EqualValues(t, 2, handshakes.Load())
			require.EqualValues(t, 1, posts.Load())
			require.EqualValues(t, 1, frames.Load())
		})
	}
}
