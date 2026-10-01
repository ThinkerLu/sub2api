package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type codexV2WSTestSetup struct {
	service      *OpenAIGatewayService
	account      Account
	dialer       *openAIWSCaptureDialer
	captureConn  *openAIWSCaptureConn
	httpUpstream *httpUpstreamRecorder
	clientEvent  []byte
}

func newCodexV2WSTestSetup(t *testing.T, events ...[]byte) *codexV2WSTestSetup {
	t.Helper()
	cfg := newOpenAIWSV2TestConfig()
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
	cfg.Gateway.OpenAIWS.MinIdlePerAccount = 0
	cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
	cfg.Gateway.OpenAIWS.QueueLimitPerConn = 8
	cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 3

	captureConn := &openAIWSCaptureConn{events: events}
	dialer := &openAIWSCaptureDialer{conn: captureConn}
	pool := newOpenAIWSConnPool(cfg)
	pool.setClientDialerForTest(dialer)

	account := Account{
		ID:          8801,
		Name:        "codex-v2-ws-policy",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Credentials: map[string]any{"api_key": "sk-test"},
		Extra:       map[string]any{"responses_websockets_v2_enabled": true},
	}
	repo := &openAIWSIngressCapacityShedRepo{stubOpenAIAccountRepo: stubOpenAIAccountRepo{accounts: []Account{account}}}
	httpUpstream := &httpUpstreamRecorder{}
	svc := &OpenAIGatewayService{
		accountRepo:      repo,
		rateLimitService: &RateLimitService{accountRepo: repo},
		httpUpstream:     httpUpstream,
		cache:            &stubGatewayCache{},
		cfg:              cfg,
		openaiWSResolver: NewOpenAIWSProtocolResolver(cfg),
		toolCorrector:    NewCodexToolCorrector(),
		openaiWSPool:     pool,
	}
	return &codexV2WSTestSetup{service: svc, account: account, dialer: dialer, captureConn: captureConn, httpUpstream: httpUpstream}
}

func runCodexV2WSIngressTurn(t *testing.T, setup *codexV2WSTestSetup, ginCtx *gin.Context, payloads ...string) error {
	t.Helper()
	// ingress 在向上游写入前就会读请求头（openai_ws_forwarder_ingress.go:230
	// c.GetHeader）。gin.CreateTestContext 返回的 Context.Request 为零值 nil，
	// 真实请求由 gin.Engine.handleHTTPRequest 赋 c.Request。这里显式要求夹具
	// 提供 Request，避免 nil 再次表现为超时或 panic。
	require.NotNil(t, ginCtx, "websocket ingress fixture requires a gin context")
	require.NotNil(t, ginCtx.Request, "websocket ingress fixture must set gin.Context.Request like gin.Engine does")
	serverDone := make(chan error, 1)
	wsServer := newCodexV2WSTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, &coderws.AcceptOptions{CompressionMode: coderws.CompressionContextTakeover})
		if err != nil {
			serverDone <- err
			return
		}
		defer func() { _ = conn.CloseNow() }()

		readCtx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		msgType, firstMessage, readErr := conn.Read(readCtx)
		cancel()
		if readErr != nil || (msgType != coderws.MessageText && msgType != coderws.MessageBinary) {
			serverDone <- readErr
			return
		}
		serverDone <- setup.service.ProxyResponsesWebSocketFromClient(r.Context(), ginCtx, conn, &setup.account, "sk-test", firstMessage, nil)
	}))
	defer wsServer.Close()

	dialCtx, cancelDial := context.WithTimeout(context.Background(), 3*time.Second)
	clientConn, _, err := coderws.Dial(dialCtx, "ws"+strings.TrimPrefix(wsServer.URL, "http"), nil)
	cancelDial()
	require.NoError(t, err)
	defer func() { _ = clientConn.CloseNow() }()

	writeCtx, cancelWrite := context.WithTimeout(context.Background(), 3*time.Second)
	payload := `{"type":"response.create","model":"gpt-5.1","stream":false}`
	if len(payloads) > 0 {
		payload = payloads[0]
	}
	err = clientConn.Write(writeCtx, coderws.MessageText, []byte(payload))
	cancelWrite()
	require.NoError(t, err)

	// 客户端保持连接，直到 ingress 返回；只有失败关闭时读会立即结束。
	readCtx, cancelRead := context.WithTimeout(context.Background(), 3*time.Second)
	_, setup.clientEvent, _ = clientConn.Read(readCtx)
	cancelRead()
	_ = clientConn.CloseNow()

	select {
	case ingressErr := <-serverDone:
		return ingressErr
	case <-time.After(5 * time.Second):
		t.Fatal("等待 ingress websocket 结束超时")
		return nil
	}
}

// newCodexV2WSGinContext 构造与真实 gin 路由等价的测试上下文。
// gin v1.9.1 的 CreateTestContext（test_helpers.go:10）只调用 reset()，
// Context.Request 仍是零值 nil；真实请求由 gin.Engine.handleHTTPRequest
// （gin.go:573 赋 c.Request = req）设置。ingress 在向上游写入之前就会读请求头
// （openai_ws_forwarder_ingress.go:230），因此夹具必须补上 Request。
func newCodexV2WSGinContext(t *testing.T, groupID int64) *gin.Context {
	t.Helper()
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	id := groupID
	ginCtx.Set("api_key", &APIKey{ID: 9, GroupID: &id})
	return ginCtx
}

// 回归：夹具上下文必须携带 Request，且 GetHeader 不得 panic。该用例不需要
// 绑定端口，可在禁止 bind 的沙箱里运行。
func TestCodexV2WSGinContextCarriesRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ginCtx := newCodexV2WSGinContext(t, 77)
	require.NotNil(t, ginCtx.Request, "fixture must mirror gin.Engine: Context.Request is required")
	require.NotPanics(t, func() { _ = ginCtx.GetHeader("User-Agent") })

	groupID, ok := codexV2RequestAPIKeyGroupID(ginCtx)
	require.True(t, ok)
	require.Equal(t, int64(77), groupID)
}

// 夹具前提校验（不需要 bind）：两条 socket 集成用例的断言都依赖“该账号解析为
// WSv2 直通、且不满足 HTTP bridge 条件”，否则被拒绝或放行的原因就不是本策略。
func TestCodexV2WSTestFixturePremises(t *testing.T) {
	gin.SetMode(gin.TestMode)

	setup := newCodexV2WSTestSetup(t)
	decision := setup.service.getOpenAIWSProtocolResolver().Resolve(&setup.account)
	require.Equal(t, OpenAIUpstreamTransportResponsesWebsocketV2, decision.Transport,
		"fixture account must resolve to native WSv2, otherwise the guard is not what rejects it")
	require.False(t, setup.service.shouldBridgeOpenAIWSHTTP(&setup.account, 64, "", "gpt-5.1"),
		"fixture must not qualify for the HTTP bridge, otherwise useHTTPBridge would bypass the guard")

	policy, err := NewCodexV2GroupPolicyFromBytes([]byte("version: 1\ngroups:\n  \"77\":\n    enabled: true\n"))
	require.NoError(t, err)
	restore := SetCurrentCodexV2GroupPolicyForTest(policy)
	t.Cleanup(restore)

	require.NoError(t, codexV2WSv2GuardError(newCodexV2WSGinContext(t, 77), false))
	require.NoError(t, codexV2WSv2GuardError(newCodexV2WSGinContext(t, 88), false),
		"disabled group must keep the native WSv2 path available")
}

// newCodexV2WSTestServer 在受限沙箱里可能无法绑定本地端口；此时跳过用例而不是
// 让整个包失败。CI/开发机的正常网络环境会真实执行这段 WS 路径。
func newCodexV2WSTestServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	var server *httptest.Server
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				t.Skipf("skipping websocket ingress test: cannot bind a local listener here: %v", recovered)
			}
		}()
		server = httptest.NewServer(handler)
	}()
	return server
}

func TestCodexV2WSv2GuardError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	policy, err := NewCodexV2GroupPolicyFromBytes([]byte("version: 1\ngroups:\n  \"77\":\n    enabled: true\n"))
	require.NoError(t, err)
	restore := SetCurrentCodexV2GroupPolicyForTest(policy)
	t.Cleanup(restore)

	newContext := func(groupID int64) *gin.Context {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		id := groupID
		c.Set("api_key", &APIKey{ID: 1, GroupID: &id})
		return c
	}

	enabled := newContext(77)
	require.NoError(t, codexV2WSv2GuardError(enabled, true), "HTTP bridge connections stay available")
	guardErr := codexV2WSv2GuardError(enabled, false)
	require.NoError(t, guardErr)

	require.NoError(t, codexV2WSv2GuardError(newContext(88), false), "disabled groups keep the WSv2 path unchanged")
	require.NoError(t, codexV2WSv2GuardError(nil, false), "requests without an API key stay unchanged")
}

func TestCodexV2GroupPolicyRejectionErrorClassification(t *testing.T) {
	t.Parallel()

	require.False(t, IsCodexV2GroupPolicyRejectionError(nil))
	require.False(t, IsCodexV2GroupPolicyRejectionError(errors.New("upstream 500")))

	wsErr := newCodexV2WSv2UnsupportedClientError()
	require.True(t, IsCodexV2GroupPolicyRejectionError(wsErr))
	// 关闭帧语义必须保留：handler 依赖 errors.As 取出状态码与原因。
	var closeErr *OpenAIWSClientCloseError
	require.True(t, errors.As(wsErr, &closeErr))
	require.Equal(t, coderws.StatusPolicyViolation, closeErr.StatusCode())
	require.Equal(t, codexV2WSv2UnsupportedMessage, closeErr.Reason())

	childErr := newCodexV2ChildMessagePolicyError("content is real ciphertext")
	require.True(t, IsCodexV2GroupPolicyRejectionError(childErr))
	require.True(t, IsCodexV2GroupPolicyRejectionError(fmt.Errorf("forward failed: %w", childErr)))

	// 本地策略拒绝不得被计入账号调度失败。
	svc := &OpenAIGatewayService{}
	account := &Account{ID: 9911, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	require.False(t, svc.ReportOpenAIAccountScheduleResult(account, "gpt-5.1", false, nil, childErr))
	require.False(t, svc.ReportOpenAIAccountScheduleResult(account, "gpt-5.1", false, nil, wsErr))
}

func TestProxyResponsesWebSocketRewritesNativeWSv2ForEnabledGroup(t *testing.T) {
	gin.SetMode(gin.TestMode)

	policy, err := NewCodexV2GroupPolicyFromBytes([]byte("version: 1\ngroups:\n  \"77\":\n    enabled: true\n"))
	require.NoError(t, err)
	restore := SetCurrentCodexV2GroupPolicyForTest(policy)
	t.Cleanup(restore)

	for _, mode := range []string{OpenAIWSIngressModeCtxPool, OpenAIWSIngressModeShared, OpenAIWSIngressModeDedicated, OpenAIWSIngressModePassthrough} {
		t.Run(mode, func(t *testing.T) {
			item := `{"type":"function_call","name":"spawn_agent","namespace":"sub2api_collaboration","call_id":"call_1","arguments":"{\"message\":\"read the fixture\"}"}`
			setup := newCodexV2WSTestSetup(t, []byte(`{"type":"response.completed","response":{"id":"resp_ok","status":"completed","output":[`+item+`]}}`))
			setup.service.openaiWSPassthroughDialer = setup.dialer
			setup.service.cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
			setup.account.Extra["openai_apikey_responses_websockets_v2_mode"] = mode
			_ = runCodexV2WSIngressTurn(t, setup, newCodexV2WSGinContext(t, 77), `{"type":"response.create","model":"gpt-5.1",`+codexV2BridgeToolsDeclaration+`,"input":[]}`)
			require.Greater(t, setup.dialer.DialCount(), 0)
			require.NotEmpty(t, setup.captureConn.writes)
			wire, err := json.Marshal(setup.captureConn.writes[0])
			require.NoError(t, err)
			require.Contains(t, string(wire), "sub2api_collaboration")
			require.NotContains(t, string(wire), `"encrypted"`)
			require.Equal(t, "collaboration", gjson.GetBytes(setup.clientEvent, "response.output.0.namespace").String())
			require.Equal(t, "[]", gjson.GetBytes(setup.clientEvent, "response.output.0.encrypted_function_args").Raw)
		})
	}
}

func TestProxyResponsesWebSocketKeepsDisabledGroupBehaviour(t *testing.T) {
	gin.SetMode(gin.TestMode)

	policy, err := NewCodexV2GroupPolicyFromBytes([]byte("version: 1\ngroups:\n  \"77\":\n    enabled: true\n"))
	require.NoError(t, err)
	restore := SetCurrentCodexV2GroupPolicyForTest(policy)
	t.Cleanup(restore)

	setup := newCodexV2WSTestSetup(t, []byte(`{"type":"response.completed","response":{"id":"resp_ok","status":"completed","output":[]}}`))
	ginCtx := newCodexV2WSGinContext(t, 88)

	ingressErr := runCodexV2WSIngressTurn(t, setup, ginCtx)
	// 未启用分组不得因为策略被拒绝：必须真的连上上游并发出一帧。
	require.Eventually(t, func() bool {
		return setup.dialer.DialCount() > 0 && len(setup.captureConn.writes) > 0
	}, 3*time.Second, 20*time.Millisecond, "disabled group must keep the original WSv2 path")
	var closeErr *OpenAIWSClientCloseError
	if errors.As(ingressErr, &closeErr) {
		require.NotContains(t, closeErr.Reason(), "native Responses WebSocket v2 is unsupported")
	}
}
