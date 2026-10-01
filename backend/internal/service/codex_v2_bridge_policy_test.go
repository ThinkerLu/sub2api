package service

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// codexV2BridgeToolsDeclaration 是父端 collaboration 声明的脱敏 fixture，
// message 带属性级 encrypted 标记。
const codexV2BridgeToolsDeclaration = `"tools":[{"type":"namespace","name":"collaboration","description":"sub-agents","tools":[` +
	`{"type":"function","name":"spawn_agent","parameters":{"type":"object","properties":{"fork_turns":{"type":"string"},"message":{"type":"string","encrypted":true},"task_name":{"type":"string"}}}}]}]`

// codexV2BridgeFunctionCallSSE 返回一段含 added/delta/done/completed 的完整 SSE，
// 其中 complete function_call 项的 arguments.message 是明文。
func codexV2BridgeFunctionCallSSE(responseID, callID string) string {
	item := `{"type":"function_call","id":"fc_` + callID + `","call_id":"` + callID + `","name":"spawn_agent","namespace":"collaboration","arguments":"{\"message\":\"run the tests\"}"}`
	delta := `{"type":"response.function_call_arguments.delta","item_id":"fc_` + callID + `","delta":"{\"message\""}`
	added := `{"type":"response.output_item.added","output_index":0,"item":` + item + `}`
	done := `{"type":"response.output_item.done","output_index":0,"item":` + item + `}`
	completed := `{"type":"response.completed","response":{"id":"` + responseID + `","model":"gpt-5.6-sol","output":[` + item + `],"usage":{"input_tokens":1,"output_tokens":1}}}`
	return strings.Join([]string{
		"data: " + added, "",
		"data: " + delta, "",
		"data: " + done, "",
		"data: " + completed, "",
	}, "\n")
}

func newCodexV2BridgeTestContext(t *testing.T, groupID int64) *gin.Context {
	t.Helper()
	c, _ := newCodexV2BridgeTestContextWithRecorder(t, groupID)
	return c
}

func newCodexV2BridgeTestContextWithRecorder(t *testing.T, groupID int64) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	id := groupID
	c.Set("api_key", &APIKey{ID: 4242, GroupID: &id})
	return c, recorder
}

func newCodexV2BridgeTestService(t *testing.T, responses ...string) (*OpenAIGatewayService, *httpUpstreamRecorder) {
	t.Helper()
	upstream := &httpUpstreamRecorder{}
	for _, body := range responses {
		upstream.responses = append(upstream.responses, &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		})
	}
	cfg := &config.Config{}
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.APIKeyEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.HTTPBridgeEnabled = true
	cfg.Gateway.OpenAIWS.HTTPBridgeThresholdBytes = 1
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 3
	return &OpenAIGatewayService{
		cfg:              cfg,
		httpUpstream:     upstream,
		cache:            &stubGatewayCache{},
		openaiWSResolver: NewOpenAIWSProtocolResolver(cfg),
		toolCorrector:    NewCodexToolCorrector(),
	}, upstream
}

func codexV2BridgeRunTurn(
	t *testing.T,
	svc *OpenAIGatewayService,
	c *gin.Context,
	account *Account,
	turn int,
	payload string,
) ([]string, error) {
	t.Helper()
	var frames []string
	_, err := svc.proxyOpenAIWSHTTPBridgeTurn(
		c.Request.Context(),
		c,
		account,
		"sk-test",
		[]byte(payload),
		len(payload),
		"gpt-5.6-sol",
		"", "", "", "",
		turn,
		func(message []byte) error {
			frames = append(frames, string(message))
			return nil
		},
	)
	return frames, err
}

func codexV2BridgeOpenAIAccount() *Account {
	return &Account{
		ID: 9001, Name: "codex-v2-bridge-parent", Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "sk-upstream"}, Extra: map[string]any{"responses_websockets_v2_enabled": true},
		Concurrency: 1, Status: StatusActive, Schedulable: true,
	}
}

func codexV2BridgeDeepSeekAccount() *Account {
	return &Account{
		ID: 9002, Name: "codex-v2-bridge-child", Platform: PlatformDeepseek, Type: AccountTypeAPIKey,
		// 复刻真实存储形态：api_protocol 在 credentials 而不是 extra，
		// 这样 GetAPIProtocol()=responses、functionToolUpstream=true。
		Credentials: map[string]any{"api_key": "sk-child", "api_protocol": APIProtocolResponses},
		Extra:       map[string]any{"responses_websockets_v2_enabled": true},
		Concurrency: 1, Status: StatusActive, Schedulable: true,
	}
}

// codexV2BridgeMarkedFrames 返回携带 encrypted_function_args 的完整调用项帧。
func codexV2BridgeMarkedFrames(frames []string) []string {
	marked := make([]string, 0, len(frames))
	for _, frame := range frames {
		item := gjson.Get(frame, "item.encrypted_function_args")
		terminal := gjson.Get(frame, "response.output.0.encrypted_function_args")
		if item.Exists() || terminal.Exists() {
			marked = append(marked, frame)
		}
	}
	return marked
}

func TestCodexV2BridgeFollowupWithoutToolsInheritsPlaintextSchema(t *testing.T) {
	policy, err := NewCodexV2GroupPolicyFromBytes([]byte("version: 1\ngroups:\n  \"77\":\n    enabled: true\n"))
	require.NoError(t, err)
	restore := SetCurrentCodexV2GroupPolicyForTest(policy)
	t.Cleanup(restore)

	svc, upstream := newCodexV2BridgeTestService(t,
		codexV2BridgeFunctionCallSSE("resp_1", "call_1"),
		codexV2BridgeFunctionCallSSE("resp_2", "call_2"),
	)
	c := newCodexV2BridgeTestContext(t, 77)
	account := codexV2BridgeOpenAIAccount()

	first, err := codexV2BridgeRunTurn(t, svc, c, account, 1,
		`{"type":"response.create","model":"gpt-5.6-sol","stream":true,`+codexV2BridgeToolsDeclaration+`,"input":"spawn a helper"}`)
	require.NoError(t, err)
	require.Len(t, upstream.bodies, 1)
	require.NotContains(t, string(upstream.bodies[0]), `"encrypted"`,
		"parent request schema must be sent without the encrypted marker")
	firstMarked := codexV2BridgeMarkedFrames(first)
	require.NotEmpty(t, firstMarked, "the complete plaintext call item must carry encrypted_function_args")
	for _, frame := range firstMarked {
		for _, path := range []string{"item.encrypted_function_args", "response.output.0.encrypted_function_args"} {
			if value := gjson.Get(frame, path); value.Exists() {
				require.Equal(t, "[]", value.Raw, "marker must be an exact empty array")
			}
		}
	}
	sawDelta := false
	for _, frame := range first {
		if gjson.Get(frame, "type").String() != "response.function_call_arguments.delta" {
			continue
		}
		sawDelta = true
		require.NotContains(t, frame, "encrypted_function_args",
			"delta events must not be rebuilt or marked")
	}
	require.True(t, sawDelta, "fixture must emit a delta event")

	// 续帧省略 tools：适配器会把上一轮已经去掉 encrypted 标记的声明赛回上游。
	second, err := codexV2BridgeRunTurn(t, svc, c, account, 2,
		`{"type":"response.create","model":"gpt-5.6-sol","stream":true,"previous_response_id":"resp_1","input":[{"type":"function_call_output","call_id":"call_1","output":"done"}]}`)
	require.NoError(t, err)
	require.Len(t, upstream.bodies, 2)
	require.NotContains(t, string(upstream.bodies[1]), `"encrypted"`,
		"the rehydrated declaration must stay without the encrypted marker")
	firstTools := gjson.GetBytes(upstream.bodies[0], "tools").Raw
	secondTools := gjson.GetBytes(upstream.bodies[1], "tools").Raw
	require.NotEmpty(t, firstTools)
	require.JSONEq(t, firstTools, secondTools,
		"the follow-up must rehydrate exactly the inherited lowered tool declaration")
	require.Contains(t, secondTools, "collaboration")
	secondMarked := codexV2BridgeMarkedFrames(second)
	require.NotEmpty(t, secondMarked,
		"the inherited plaintext schema must still produce encrypted_function_args on the round trip")
	require.Equal(t, "[]", gjson.Get(secondMarked[0], "item.encrypted_function_args").Raw)
}

func TestCodexV2BridgeReplacementToolsResetInheritedPlaintextSchema(t *testing.T) {
	policy, err := NewCodexV2GroupPolicyFromBytes([]byte("version: 1\ngroups:\n  \"77\":\n    enabled: true\n"))
	require.NoError(t, err)
	restore := SetCurrentCodexV2GroupPolicyForTest(policy)
	t.Cleanup(restore)

	svc, upstream := newCodexV2BridgeTestService(t,
		codexV2BridgeFunctionCallSSE("resp_1", "call_1"),
		codexV2BridgeFunctionCallSSE("resp_2", "call_2"),
	)
	c := newCodexV2BridgeTestContext(t, 77)
	account := codexV2BridgeOpenAIAccount()

	_, err = codexV2BridgeRunTurn(t, svc, c, account, 1,
		`{"type":"response.create","model":"gpt-5.6-sol","stream":true,`+codexV2BridgeToolsDeclaration+`,"input":"spawn a helper"}`)
	require.NoError(t, err)

	// 显式替换成不含 collaboration 的新声明：请求与回程标记都必须重建。
	second, err := codexV2BridgeRunTurn(t, svc, c, account, 2,
		`{"type":"response.create","model":"gpt-5.6-sol","stream":true,"previous_response_id":"resp_1",`+
			`"tools":[{"type":"function","name":"shell","parameters":{"type":"object","properties":{"command":{"type":"string"}}}}],`+
			`"input":[{"type":"function_call_output","call_id":"call_1","output":"done"}]}`)
	require.NoError(t, err)
	require.Len(t, upstream.bodies, 2)
	require.NotContains(t, string(upstream.bodies[1]), "collaboration",
		"explicit replacement tools must not rehydrate the inherited declaration")
	require.Equal(t, "shell", gjson.GetBytes(upstream.bodies[1], "tools.0.name").String())
	// 模型仍然回了一个（不该出现的）裸名协作调用：这里专门验证标记已随替换清零，
	// 不会沿用上一轮的“已去加密”元数据。
	require.Empty(t, codexV2BridgeMarkedFrames(second),
		"replacement tools must clear the inherited plaintext-schema marker")
}

func TestCodexV2BridgeAccountFailoverDoesNotReuseParentMarker(t *testing.T) {
	policy, err := NewCodexV2GroupPolicyFromBytes([]byte("version: 1\ngroups:\n  \"77\":\n    enabled: true\n"))
	require.NoError(t, err)
	restore := SetCurrentCodexV2GroupPolicyForTest(policy)
	t.Cleanup(restore)

	svc, upstream := newCodexV2BridgeTestService(t,
		codexV2BridgeFunctionCallSSE("resp_1", "call_1"),
		codexV2BridgeFunctionCallSSE("resp_2", "call_2"),
	)
	c := newCodexV2BridgeTestContext(t, 77)

	_, err = codexV2BridgeRunTurn(t, svc, c, codexV2BridgeOpenAIAccount(), 1,
		`{"type":"response.create","model":"gpt-5.6-sol","stream":true,`+codexV2BridgeToolsDeclaration+`,"input":"spawn a helper"}`)
	require.NoError(t, err)
	require.True(t, codexV2ParentPlaintextAttemptActive(c))

	// 同一连接换到非 OpenAI 账号，按保存的客户端声明重建别名，而不是继承旧派生体。
	childFrames, err := codexV2BridgeRunTurn(t, svc, c, codexV2BridgeDeepSeekAccount(), 2,
		`{"type":"response.create","model":"deepseek-flash","stream":true,"input":[{"type":"agent_message","id":"amsg_1","author":"/root","recipient":"/root/task","content":[{"type":"input_text","text":"envelope"},{"type":"encrypted_content","encrypted_content":"plain task payload"}]}]}`)
	require.NoError(t, err)
	require.True(t, codexV2ParentPlaintextAttemptActive(c))
	require.NotEmpty(t, codexV2BridgeMarkedFrames(childFrames))
	require.Len(t, upstream.bodies, 2)
	require.NotContains(t, string(upstream.bodies[1]), "agent_message",
		"the child-side plaintext agent_message must be rewritten before the upstream call")
	require.Contains(t, string(upstream.bodies[1]), `"role":"user"`,
		"the child-side rewrite must produce a plain user message")
	// 非 OpenAI 账号也使用别名声明与明文回程，连接仍保存未修改的客户端来源。
	require.NotContains(t, string(upstream.bodies[1]), `"encrypted":true`)
	require.Contains(t, string(upstream.bodies[1]), "sub2api_collaboration")
	state, ok := openAIWSHTTPBridgeToolStateFromContext(c)
	require.True(t, ok)
	require.Contains(t, string(state.ClientTools), `"encrypted":true`)
}

func TestCodexV2BridgeConnectionPolicySnapshotIsPinned(t *testing.T) {
	enabled, err := NewCodexV2GroupPolicyFromBytes([]byte("version: 1\ngroups:\n  \"77\":\n    enabled: true\n"))
	require.NoError(t, err)
	restore := SetCurrentCodexV2GroupPolicyForTest(enabled)
	t.Cleanup(restore)

	svc, upstream := newCodexV2BridgeTestService(t,
		codexV2BridgeFunctionCallSSE("resp_1", "call_1"),
		codexV2BridgeFunctionCallSSE("resp_2", "call_2"),
	)
	c := newCodexV2BridgeTestContext(t, 77)
	account := codexV2BridgeOpenAIAccount()

	_, err = codexV2BridgeRunTurn(t, svc, c, account, 1,
		`{"type":"response.create","model":"gpt-5.6-sol","stream":true,`+codexV2BridgeToolsDeclaration+`,"input":"spawn a helper"}`)
	require.NoError(t, err)

	// 连接建立后热重载关闭该分组：本连接仍按首次固定的快照处理。
	disabled, err := NewCodexV2GroupPolicyFromBytes([]byte("version: 1\ngroups:\n  \"77\":\n    enabled: false\n"))
	require.NoError(t, err)
	SetCurrentCodexV2GroupPolicyForTest(disabled)

	second, err := codexV2BridgeRunTurn(t, svc, c, account, 2,
		`{"type":"response.create","model":"gpt-5.6-sol","stream":true,"previous_response_id":"resp_1",`+codexV2BridgeToolsDeclaration+`,"input":[{"type":"function_call_output","call_id":"call_1","output":"done"}]}`)
	require.NoError(t, err)
	require.Len(t, upstream.bodies, 2)
	require.NotContains(t, string(upstream.bodies[1]), `"encrypted"`,
		"an established connection must keep its pinned policy snapshot")
	require.NotEmpty(t, codexV2BridgeMarkedFrames(second),
		"the pinned snapshot must keep injecting the round-trip marker")
}

func TestCodexV2BridgeReconnectDoesNotInheritToolState(t *testing.T) {
	policy, err := NewCodexV2GroupPolicyFromBytes([]byte("version: 1\ngroups:\n  \"77\":\n    enabled: true\n"))
	require.NoError(t, err)
	restore := SetCurrentCodexV2GroupPolicyForTest(policy)
	t.Cleanup(restore)

	svc, upstream := newCodexV2BridgeTestService(t,
		codexV2BridgeFunctionCallSSE("resp_1", "call_1"),
		codexV2BridgeFunctionCallSSE("resp_2", "call_2"),
	)
	account := codexV2BridgeOpenAIAccount()

	firstConn := newCodexV2BridgeTestContext(t, 77)
	_, err = codexV2BridgeRunTurn(t, svc, firstConn, account, 1,
		`{"type":"response.create","model":"gpt-5.6-sol","stream":true,`+codexV2BridgeToolsDeclaration+`,"input":"spawn a helper"}`)
	require.NoError(t, err)

	// 新连接（重连）省略 tools：不得继承上一连接的声明与标记。
	reconnected := newCodexV2BridgeTestContext(t, 77)
	frames, err := codexV2BridgeRunTurn(t, svc, reconnected, account, 1,
		`{"type":"response.create","model":"gpt-5.6-sol","stream":true,"input":[{"type":"function_call_output","call_id":"call_1","output":"done"}]}`)
	require.NoError(t, err)
	require.Len(t, upstream.bodies, 2)
	require.NotContains(t, string(upstream.bodies[1]), "collaboration",
		"a reconnected session must not rehydrate the previous connection's tool declaration")
	require.Empty(t, codexV2BridgeMarkedFrames(frames),
		"a reconnected session must not inherit the previous connection's marker")
}
