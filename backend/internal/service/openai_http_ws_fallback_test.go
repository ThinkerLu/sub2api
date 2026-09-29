package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestHTTPWSHandshakeFallbackAdmission(t *testing.T) {
	for _, tc := range []struct {
		status  int
		allowed bool
	}{
		{0, true}, {404, true}, {405, true}, {426, true}, {500, true}, {501, true}, {502, true}, {503, true}, {504, true},
		{101, false}, {200, false}, {400, false}, {401, false}, {403, false}, {429, false},
	} {
		s, account, _, _ := adaptiveFixture()
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
		dial := &openAIWSDialError{StatusCode: tc.status, Err: io.EOF}
		err := wrapOpenAIWSFallback(classifyOpenAIWSAcquireError(dial), dial)
		body := []byte(`{"model":"gpt-5.1","input":[{"role":"user","content":"full synthetic context"}]}`)
		require.Equal(t, tc.allowed, s.canFallbackHTTPAfterWSHandshake(context.Background(), c, account, body, err), "status=%d", tc.status)
	}
	for _, scenario := range []string{"canceled", "write_attempted", "response_written", "ws_ingress", "old_id", "orphan", "no_input", "warmup", "pool_full", "wrong_stage", "explicit_passthrough", "incompatible_http", "quota_disguised_as_503"} {
		t.Run(scenario, func(t *testing.T) {
			s, a, _, _ := adaptiveFixture()
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			body := []byte(`{"model":"gpt-5.1","input":"full synthetic context"}`)
			dial := &openAIWSDialError{StatusCode: 426, Err: io.EOF}
			err := wrapOpenAIWSFallback("upgrade_required", dial)
			switch scenario {
			case "canceled":
				cancel()
			case "write_attempted":
				c.Set(openAIHTTPWSWriteAttemptedKey, true)
			case "response_written":
				c.String(200, "started")
			case "ws_ingress":
				SetOpenAIClientTransport(c, OpenAIClientTransportWS)
			case "old_id":
				body = []byte(`{"input":"full","previous_response_id":"resp_old"}`)
			case "orphan":
				body = []byte(`{"input":[{"type":"function_call_output","call_id":"orphan","output":"done"}]}`)
			case "no_input":
				body = []byte(`{"model":"gpt-5.1"}`)
			case "warmup":
				body = []byte(`{"input":"full","generate":false}`)
			case "pool_full":
				err = wrapOpenAIWSFallback("conn_queue_full", errOpenAIWSConnQueueFull)
			case "wrong_stage":
				err = wrapOpenAIWSFallback("read_event", dial)
			case "explicit_passthrough":
				a.Extra["openai_apikey_responses_websockets_v2_mode"] = OpenAIWSIngressModePassthrough
			case "incompatible_http":
				a.Platform = PlatformGrok
			case "quota_disguised_as_503":
				dial.StatusCode = 503
				dial.ResponseBody = []byte(`{"error":{"code":"insufficient_quota","message":"quota exhausted"}}`)
				err = wrapOpenAIWSFallback("upstream_5xx", dial)
			}
			require.False(t, s.canFallbackHTTPAfterWSHandshake(ctx, c, a, body, err))
		})
	}
}

func TestHTTPWSFallbackHTTPFailureCannotReplayOrFailover(t *testing.T) {
	for _, rejection := range []bool{false, true} {
		t.Run(map[bool]string{false: "transport_error", true: "rejected_field"}[rejection], func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "not supported", 426) }))
			defer up.Close()
			s, a, _, _ := adaptiveFixture()
			a.Credentials["base_url"] = up.URL
			httpUp := &httpUpstreamSequenceRecorder{errs: []error{io.ErrUnexpectedEOF}}
			if rejection {
				httpUp.errs = nil
				httpUp.responses = []*http.Response{{StatusCode: 400, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"Unsupported parameter: 'truncation'.","type":"invalid_request_error","param":"truncation","code":"unsupported_parameter"}}`))}}
			}
			s.httpUpstream = httpUp
			defer s.CloseOpenAIWSPool()
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
			SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
			_, err := s.Forward(context.Background(), c, a, []byte(`{"model":"gpt-5.1","stream":false,"input":"full context","truncation":"auto"}`))
			require.Error(t, err)
			require.Contains(t, err.Error(), "automatic replay is disabled")
			require.Equal(t, http.StatusBadRequest, c.Writer.Status())
			var failover *UpstreamFailoverError
			require.False(t, errors.As(err, &failover), "handler must not resubmit to another account")
			require.Equal(t, 1, httpUp.callCount)
			require.Equal(t, "ws_handshake_fallback", c.GetString("openai_ws_transport_reason"))
		})
	}
}
