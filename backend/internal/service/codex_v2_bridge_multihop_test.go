package service

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// 复核者复现的多跳序列（同一 WS 连接、启用组、WS→HTTP bridge）：
// 父端(带 collaboration tools) → 非 OpenAI 原生 Responses 子端(省 tools) → 父端(省 tools)。
//
// 修复前：第 2 轮把上一账号派生出的“已去 encrypted”声明塞给子端上游但标记被清，
// 第 3 轮又把这份去标记声明塞回父端而标记仍是 false，回程不补
// encrypted_function_args: []，请求侧与回程契约不对称。修复后：连接保存客户端原始
// 声明，每轮按实际选中的账号重新推导上游声明与回程标记。
func TestCodexV2BridgeMultiHopRederivesPerAccount(t *testing.T) {
	policy, err := NewCodexV2GroupPolicyFromBytes([]byte("version: 1\ngroups:\n  \"77\":\n    enabled: true\n"))
	require.NoError(t, err)
	restore := SetCurrentCodexV2GroupPolicyForTest(policy)
	t.Cleanup(restore)

	svc, upstream := newCodexV2BridgeTestService(t,
		codexV2BridgeFunctionCallSSE("resp_1", "call_1"),
		codexV2BridgeFunctionCallSSE("resp_2", "call_2"),
		codexV2BridgeFunctionCallSSE("resp_3", "call_3"),
	)
	c := newCodexV2BridgeTestContext(t, 77)

	// 第 1 轮：父端（OpenAI 平台）带 collaboration 工具声明。
	first, err := codexV2BridgeRunTurn(t, svc, c, codexV2BridgeOpenAIAccount(), 1,
		`{"type":"response.create","model":"gpt-5.6-sol","stream":true,`+codexV2BridgeToolsDeclaration+`,"input":"spawn a helper"}`)
	require.NoError(t, err)
	require.Len(t, upstream.bodies, 1)
	require.NotContains(t, string(upstream.bodies[0]), `"encrypted"`)
	require.NotEmpty(t, codexV2BridgeMarkedFrames(first))
	state, ok := openAIWSHTTPBridgeToolStateFromContext(c)
	require.True(t, ok)
	require.True(t, state.CodexV2ParentPlaintextSchema)
	require.Contains(t, string(state.ClientTools), `"encrypted":true`,
		"the connection must keep the client-form declaration as its source")

	// 第 2 轮：失败尝试切到非 OpenAI、原生 Responses 的子端账号（api_protocol 在
	// Credentials），本帧省略 tools。
	childFrames, err := codexV2BridgeRunTurn(t, svc, c, codexV2BridgeDeepSeekAccount(), 2,
		`{"type":"response.create","model":"deepseek-flash","stream":true,"previous_response_id":"resp_1","input":[{"type":"function_call_output","call_id":"call_1","output":"done"}]}`)
	require.NoError(t, err)
	require.Len(t, upstream.bodies, 2)
	require.NotContains(t, string(upstream.bodies[1]), `"encrypted":true`)
	require.Contains(t, string(upstream.bodies[1]), "sub2api_collaboration")
	require.NotEmpty(t, codexV2BridgeMarkedFrames(childFrames))
	state, ok = openAIWSHTTPBridgeToolStateFromContext(c)
	require.True(t, ok)
	require.True(t, state.CodexV2ParentPlaintextSchema)

	// 第 3 轮：切回父端，本帧仍省略 tools。上游必须收到去标记声明，且回程补 []。
	third, err := codexV2BridgeRunTurn(t, svc, c, codexV2BridgeOpenAIAccount(), 3,
		`{"type":"response.create","model":"gpt-5.6-sol","stream":true,"previous_response_id":"resp_2","input":[{"type":"function_call_output","call_id":"call_2","output":"done"}]}`)
	require.NoError(t, err)
	require.Len(t, upstream.bodies, 3)
	require.NotContains(t, string(upstream.bodies[2]), `"encrypted"`,
		"the parent attempt must send the stripped declaration again")
	marked := codexV2BridgeMarkedFrames(third)
	require.NotEmpty(t, marked,
		"returning to a parent account must restore the encrypted_function_args marker")
	require.Equal(t, "[]", gjson.Get(marked[0], "item.encrypted_function_args").Raw)
	state, ok = openAIWSHTTPBridgeToolStateFromContext(c)
	require.True(t, ok)
	require.True(t, state.CodexV2ParentPlaintextSchema)
	require.Contains(t, string(state.ClientTools), `"encrypted":true`,
		"the client-form source must survive every hop unchanged")
}

// Lite 载体：collaboration 声明放在 input[].additional_tools[].tools[] 且省略顶层
// tools 时，回程标记必须由本帧实际出站声明（而不是上一轮的继承值）决定。
func TestCodexV2BridgeLiteAdditionalToolsCarrierKeepsMarkerInSync(t *testing.T) {
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

	// 第 1 轮：普通顶层声明，建立连接状态。
	_, err = codexV2BridgeRunTurn(t, svc, c, account, 1,
		`{"type":"response.create","model":"gpt-5.6-sol","stream":true,`+codexV2BridgeToolsDeclaration+`,"input":"spawn a helper"}`)
	require.NoError(t, err)

	// 第 2 轮：Lite 载体显式声明 collaboration，顶层 tools 省略。
	liteCarrier := `"input":[{"type":"additional_tools","tools":[{` +
		`"type":"namespace","name":"collaboration","tools":[{"type":"function","name":"spawn_agent",` +
		`"parameters":{"type":"object","properties":{"message":{"type":"string","encrypted":true}}}}]}]},` +
		`{"type":"function_call_output","call_id":"call_1","output":"done"}]`
	litePayload := `{"type":"response.create","model":"gpt-5.6-sol","stream":true,"previous_response_id":"resp_1",` +
		`"client_metadata":{"` + responsesLiteWSMetadataKey + `":"true"},` + liteCarrier + `}`

	second, err := codexV2BridgeRunTurn(t, svc, c, account, 2, litePayload)
	require.NoError(t, err)
	require.Len(t, upstream.bodies, 2)
	require.True(t, isOpenAIResponsesLiteWebSocketPayload(upstream.bodies[1]),
		"the fixture must stay on the Lite carrier path")
	require.False(t, gjson.GetBytes(upstream.bodies[1], "input.0.tools.0.tools.0.parameters.properties.message.encrypted").Exists(),
		"the Lite carrier declaration must be sent without the encrypted marker")
	marked := codexV2BridgeMarkedFrames(second)
	require.NotEmpty(t, marked,
		"the Lite carrier declaration must drive the round-trip marker")
	require.Equal(t, "[]", gjson.Get(marked[0], "item.encrypted_function_args").Raw)

	state, ok := openAIWSHTTPBridgeToolStateFromContext(c)
	require.True(t, ok)
	require.True(t, state.CodexV2ParentPlaintextSchema)
}

// 连接里存在继承来的上游声明、却缺少客户端原始来源时，无法按本次账号安全重建；
// 必须在任何上游写入之前失败关闭，而不是发出半套请求。
func TestCodexV2BridgeUnsafeInheritedDeclarationFailsClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)

	policy, err := NewCodexV2GroupPolicyFromBytes([]byte("version: 1\ngroups:\n  \"77\":\n    enabled: true\n"))
	require.NoError(t, err)
	restore := SetCurrentCodexV2GroupPolicyForTest(policy)
	t.Cleanup(restore)

	svc, upstream := newCodexV2BridgeTestService(t)
	c := newCodexV2BridgeTestContext(t, 77)

	// 只留下派生的上游声明与映射，故意不提供客户端原始声明。
	setOpenAIWSHTTPBridgeToolState(c, openAIWSHTTPBridgeToolState{
		ClientMapping: apicompat.ResponsesClientToolMapping{
			NamespaceTools: map[string]apicompat.ResponsesNamespaceName{
				"collaboration__spawn_agent": {Namespace: "collaboration", Name: "spawn_agent"},
			},
		},
		LoweredTools: []byte(`[{"type":"function","name":"collaboration__spawn_agent","parameters":{"type":"object","properties":{"message":{"type":"string"}}}}]`),
	})

	var frames []string
	payload := `{"type":"response.create","model":"gpt-5.6-sol","stream":true,"input":[{"type":"function_call_output","call_id":"call_1","output":"done"}]}`
	_, turnErr := svc.proxyOpenAIWSHTTPBridgeTurn(
		c.Request.Context(), c, codexV2BridgeOpenAIAccount(), "sk-test",
		[]byte(payload),
		len(payload), "gpt-5.6-sol", "", "", "", "", 2,
		func(message []byte) error {
			frames = append(frames, string(message))
			return nil
		},
	)
	require.Error(t, turnErr)
	require.True(t, IsCodexV2GroupPolicyRejectionError(turnErr),
		"unsafe inheritance must be a local group-policy rejection, got %v", turnErr)
	require.Empty(t, upstream.bodies, "fail-closed must not reach any upstream write")
	require.Len(t, frames, 1, "the client must receive exactly one explicit error frame")
	require.Contains(t, frames[0], codexV2UnsafeToolInheritanceMessage)
}

// 失败尝试（上游 5xx → failover）之后，连接保存的客户端原始来源不得被破坏：
// 下一次父端尝试省略 tools 时仍必须发出去标记声明并补 []。
func TestCodexV2BridgeFailedAttemptRetryKeepsClientSource(t *testing.T) {
	policy, err := NewCodexV2GroupPolicyFromBytes([]byte("version: 1\ngroups:\n  \"77\":\n    enabled: true\n"))
	require.NoError(t, err)
	restore := SetCurrentCodexV2GroupPolicyForTest(policy)
	t.Cleanup(restore)

	svc, upstream := newCodexV2BridgeTestService(t)
	upstream.responses = append(upstream.responses,
		&http.Response{
			StatusCode: http.StatusInternalServerError,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"server_error","message":"upstream exploded"}}`)),
		},
		&http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(codexV2BridgeFunctionCallSSE("resp_retry", "call_retry"))),
		},
	)
	c := newCodexV2BridgeTestContext(t, 77)
	account := codexV2BridgeOpenAIAccount()

	failedFrames, failedErr := codexV2BridgeRunTurn(t, svc, c, account, 1,
		`{"type":"response.create","model":"gpt-5.6-sol","stream":true,`+codexV2BridgeToolsDeclaration+`,"input":"spawn a helper"}`)
	require.Error(t, failedErr, "the 5xx attempt must fail and be available for failover/retry")
	require.Empty(t, codexV2BridgeMarkedFrames(failedFrames))
	state, ok := openAIWSHTTPBridgeToolStateFromContext(c)
	require.True(t, ok)
	require.Contains(t, string(state.ClientTools), `"encrypted":true`,
		"a failed attempt must keep the client-form declaration source intact")

	// 重试：省略 tools 的父端请求仍必须拿到去标记声明并补 []。
	retried, retryErr := codexV2BridgeRunTurn(t, svc, c, account, 2,
		`{"type":"response.create","model":"gpt-5.6-sol","stream":true,"input":[{"type":"function_call_output","call_id":"call_1","output":"done"}]}`)
	require.NoError(t, retryErr)
	require.Len(t, upstream.bodies, 2)
	require.NotContains(t, string(upstream.bodies[1]), `"encrypted"`,
		"the retried parent attempt must still send the stripped declaration")
	require.NotEmpty(t, codexV2BridgeMarkedFrames(retried),
		"the retried parent attempt must still carry encrypted_function_args")
}
