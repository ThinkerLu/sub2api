package handler

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestHTTPWSHandlerRecordsActualTransports(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			frames := make(chan []byte, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := coderws.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer conn.CloseNow()
				ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
				defer cancel()
				_, frame, err := conn.Read(ctx)
				if err != nil {
					return
				}
				frames <- frame
				_ = conn.Write(ctx, coderws.MessageText, []byte(`{"type":"response.completed","response":{"id":"resp_handler_http_ws","model":"deepseek-v4-pro","status":"completed","output":[],"usage":{"input_tokens":3,"output_tokens":2}}}`))
			}))
			defer upstream.Close()
			h := newOpenAIWSPassthroughHandlerHarness(t, upstream.URL, func(cfg *config.Config, a *service.Account) {
				a.Extra["openai_apikey_responses_websockets_v2_mode"] = service.OpenAIWSIngressModeAdaptive
				a.Extra["openai_passthrough"] = true
				a.Credentials["model_mapping"] = map[string]any{"deepseek-v4-pro": "deepseek-v4-pro"}
			})
			defer h.gatewayService.CloseOpenAIWSPool()
			_ = h.clientConn.CloseNow()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.serverURL+"/openai/v1/responses", strings.NewReader(fmt.Sprintf(`{"model":"deepseek-v4-pro","stream":%v,"input":[{"role":"user","content":"synthetic"}]}`, stream)))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
			if stream {
				require.Contains(t, resp.Header.Get("Content-Type"), "text/event-stream")
			} else {
				require.Equal(t, "resp_handler_http_ws", gjson.GetBytes(body, "id").String())
			}
			select {
			case frame := <-frames:
				require.Equal(t, "response.create", gjson.GetBytes(frame, "type").String())
			case <-ctx.Done():
				t.Fatal("upstream received no WS request")
			}
			select {
			case usage := <-h.usageRepo.created:
				require.EqualValues(t, service.UsageIngressTransportHTTP, usage.IngressTransport)
				require.EqualValues(t, service.UsageUpstreamTransportWebSocket, usage.UpstreamTransport)
				require.Equal(t, 3, usage.InputTokens)
				require.Equal(t, 2, usage.OutputTokens)
			case <-ctx.Done():
				t.Fatal("handler did not record usage")
			}
		})
	}
}
