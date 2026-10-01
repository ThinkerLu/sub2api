package service

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// codexV2BridgeOAuthAccount 是纯 HTTP bridge 上的 OpenAI OAuth 账号：平台是 OpenAI、
// 类型是 OAuth，因此 functionToolUpstream 为 false（不做 function-tool 降级），但启用组
// 仍需要保存/替换客户端原始工具声明。
func codexV2BridgeOAuthAccount() *Account {
	return &Account{
		ID: 9003, Name: "codex-v2-bridge-oauth", Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "oauth-token"},
		Extra:       map[string]any{"responses_websockets_v2_enabled": true},
		Concurrency: 1, Status: StatusActive, Schedulable: true,
	}
}

func codexV2BridgeTopLevelShellDeclaration() string {
	return `"tools":[{"type":"function","name":"shell","parameters":{"type":"object","properties":{"command":{"type":"string"}}}}]`
}

// P2 核心序列：API-key 首轮 collaboration A → OAuth 次轮显式 shell B → API-key 第三轮
// 省略声明。修复前第 2 轮不写连接来源（functionToolUpstream=false），第 3 轮只能重放
// 第 1 轮的陈旧 A。修复后第 3 轮必须只有 B、没有 A。
func TestCodexV2BridgeOAuthExplicitDeclarationReplacesSource(t *testing.T) {
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

	// 第 1 轮：API-key，顶层 collaboration 声明 A。
	first, err := codexV2BridgeRunTurn(t, svc, c, codexV2BridgeOpenAIAccount(), 1,
		`{"type":"response.create","model":"gpt-5.6-sol","stream":true,`+codexV2BridgeToolsDeclaration+`,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"spawn a helper"}]}]}`)
	require.NoError(t, err)
	require.Len(t, upstream.bodies, 1)
	require.Contains(t, string(upstream.bodies[0]), "collaboration__spawn_agent",
		"premise: the API-key round derives the flattened collaboration tool")
	require.NotEmpty(t, codexV2BridgeMarkedFrames(first))

	// 第 2 轮：OAuth 显式换成 shell 声明 B。
	_, err = codexV2BridgeRunTurn(t, svc, c, codexV2BridgeOAuthAccount(), 2,
		`{"type":"response.create","model":"gpt-5.6-sol","stream":true,"previous_response_id":"resp_1",`+
			codexV2BridgeTopLevelShellDeclaration()+`,"input":[{"type":"function_call_output","call_id":"call_1","output":"done"}]}`)
	require.NoError(t, err)
	require.Len(t, upstream.bodies, 2)
	require.NotContains(t, string(upstream.bodies[1]), "collaboration",
		"the OAuth round must send only the explicit replacement declaration")
	require.Contains(t, string(upstream.bodies[1]), `"name":"shell"`)
	state, ok := openAIWSHTTPBridgeToolStateFromContext(c)
	require.True(t, ok, "an OAuth round must write the connection source state")
	require.NotContains(t, string(state.LoweredTools), "collaboration",
		"the connection source must no longer carry the API-key round's collaboration declaration")
	require.Contains(t, string(state.ClientTools), `"name":"shell"`,
		"the client-form source must be replaced by the OAuth round's declaration")

	// 第 3 轮：API-key 省略声明，必须重建 B 而不是 A。
	thirdFrames, err := codexV2BridgeRunTurn(t, svc, c, codexV2BridgeOpenAIAccount(), 3,
		codexV2BridgeOmitToolsPayload("gpt-5.6-sol", "resp_2",
			`{"type":"function_call_output","call_id":"call_2","output":"done"}`))
	require.NoError(t, err)
	require.Len(t, upstream.bodies, 3)
	third := upstream.bodies[2]
	require.NotContains(t, string(third), "collaboration",
		"the stale API-key declaration must not be replayed")
	require.Contains(t, string(third), `"name":"shell"`,
		"the OAuth round's declaration must be rebuilt")
	require.Equal(t, "shell", gjson.GetBytes(third, "tools.0.name").String())
	require.Len(t, gjson.GetBytes(third, "tools").Array(), 1, "only B may be declared")
	require.Empty(t, codexV2BridgeMarkedFrames(thirdFrames),
		"a shell-only declaration must not produce encrypted_function_args")
	state, ok = openAIWSHTTPBridgeToolStateFromContext(c)
	require.True(t, ok)
	require.False(t, state.CodexV2ParentPlaintextSchema,
		"the derived marker must match the rebuilt declaration, not the stale one")
}

// 纯 OAuth 首帧声明 → 下一轮省略：声明保留且回程 marker 与出站声明一致。
func TestCodexV2BridgeOAuthFirstFrameThenOmitKeepsDeclaration(t *testing.T) {
	policy, err := NewCodexV2GroupPolicyFromBytes([]byte("version: 1\ngroups:\n  \"77\":\n    enabled: true\n"))
	require.NoError(t, err)
	restore := SetCurrentCodexV2GroupPolicyForTest(policy)
	t.Cleanup(restore)

	svc, upstream := newCodexV2BridgeTestService(t,
		codexV2BridgeFunctionCallSSE("resp_1", "call_1"),
		codexV2BridgeFunctionCallSSE("resp_2", "call_2"),
	)
	c := newCodexV2BridgeTestContext(t, 77)
	account := codexV2BridgeOAuthAccount()

	firstFrames, err := codexV2BridgeRunTurn(t, svc, c, account, 1,
		`{"type":"response.create","model":"gpt-5.6-sol","stream":true,`+codexV2BridgeToolsDeclaration+`,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"spawn a helper"}]}]}`)
	require.NoError(t, err)
	require.Len(t, upstream.bodies, 1)
	require.NotContains(t, string(upstream.bodies[0]), `"encrypted"`,
		"the OAuth parent round must strip the encrypted marker")
	require.NotEmpty(t, codexV2BridgeMarkedFrames(firstFrames))
	state, ok := openAIWSHTTPBridgeToolStateFromContext(c)
	require.True(t, ok, "a pure OAuth first frame must be recorded as the client source")
	require.Contains(t, string(state.ClientTools), `"encrypted":true`,
		"the client-form source must keep the marker")

	secondFrames, err := codexV2BridgeRunTurn(t, svc, c, account, 2,
		codexV2BridgeOmitToolsPayload("gpt-5.6-sol", "resp_1",
			`{"type":"function_call_output","call_id":"call_1","output":"done"}`))
	require.NoError(t, err)
	require.Len(t, upstream.bodies, 2)
	second := upstream.bodies[1]
	require.Equal(t, "sub2api_collaboration", gjson.GetBytes(second, "tools.0.name").String(),
		"the OAuth declaration must survive the omitted frame")
	require.False(t, gjson.GetBytes(second, "tools.0.tools.0.parameters.properties.message.encrypted").Exists(),
		"the rebuilt declaration must be stripped again on the OAuth round")
	require.NotEmpty(t, codexV2BridgeMarkedFrames(secondFrames),
		"the rebuilt declaration must produce encrypted_function_args")
	state, ok = openAIWSHTTPBridgeToolStateFromContext(c)
	require.True(t, ok)
	require.True(t, state.CodexV2ParentPlaintextSchema)
}

// OAuth 显式空声明清除旧来源：第三轮省略时不得重建任何声明。
func TestCodexV2BridgeOAuthExplicitEmptyDeclarationClearsSource(t *testing.T) {
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
	account := codexV2BridgeOAuthAccount()

	_, err = codexV2BridgeRunTurn(t, svc, c, account, 1,
		`{"type":"response.create","model":"gpt-5.6-sol","stream":true,`+codexV2BridgeToolsDeclaration+`,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"spawn a helper"}]}]}`)
	require.NoError(t, err)
	state, ok := openAIWSHTTPBridgeToolStateFromContext(c)
	require.True(t, ok)
	require.Contains(t, string(state.ClientTools), `"encrypted":true`)

	// 第二轮：显式空声明。
	_, err = codexV2BridgeRunTurn(t, svc, c, account, 2,
		`{"type":"response.create","model":"gpt-5.6-sol","stream":true,"previous_response_id":"resp_1","tools":[],"input":[{"type":"function_call_output","call_id":"call_1","output":"done"}]}`)
	require.NoError(t, err)
	state, ok = openAIWSHTTPBridgeToolStateFromContext(c)
	require.True(t, ok)
	require.Empty(t, decodeOpenAIWSHTTPBridgeRawItems(state.ClientAdditionalTools))
	require.False(t, codexV2ToolsDeclarationHasEntries(state.ClientTools),
		"an explicit empty declaration must clear the previous client-form source")
	require.False(t, codexV2ToolsDeclarationHasEntries(state.LoweredTools),
		"the derived declaration must be cleared as well")
	require.False(t, state.CodexV2ParentPlaintextSchema)

	// 第三轮：省略声明，不得重建被清掉的 collaboration。
	thirdFrames, err := codexV2BridgeRunTurn(t, svc, c, account, 3,
		codexV2BridgeOmitToolsPayload("gpt-5.6-sol", "resp_2",
			`{"type":"function_call_output","call_id":"call_2","output":"done"}`))
	require.NoError(t, err)
	require.Len(t, upstream.bodies, 3)
	require.NotContains(t, string(upstream.bodies[2]), "collaboration",
		"a cleared source must not come back")
	require.Empty(t, codexV2BridgeMarkedFrames(thirdFrames))
}

// 关闭组对照：OAuth 轮次既不保存来源也不重建，行为与基线一致。
func TestCodexV2BridgeOAuthDisabledGroupUnchanged(t *testing.T) {
	policy, err := NewCodexV2GroupPolicyFromBytes([]byte("version: 1\ngroups:\n  \"77\":\n    enabled: true\n"))
	require.NoError(t, err)
	restore := SetCurrentCodexV2GroupPolicyForTest(policy)
	t.Cleanup(restore)

	svc, upstream := newCodexV2BridgeTestService(t,
		codexV2BridgeFunctionCallSSE("resp_1", "call_1"),
		codexV2BridgeFunctionCallSSE("resp_2", "call_2"),
	)
	c := newCodexV2BridgeTestContext(t, 88)
	account := codexV2BridgeOAuthAccount()

	_, err = codexV2BridgeRunTurn(t, svc, c, account, 1,
		`{"type":"response.create","model":"gpt-5.6-sol","stream":true,`+codexV2BridgeToolsDeclaration+`,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"spawn a helper"}]}]}`)
	require.NoError(t, err)
	require.Len(t, upstream.bodies, 1)
	require.Contains(t, string(upstream.bodies[0]), `"encrypted":true`,
		"the disabled group must forward the client declaration untouched")
	require.False(t, codexV2ParentPlaintextAttemptActive(c))

	_, err = codexV2BridgeRunTurn(t, svc, c, account, 2,
		codexV2BridgeOmitToolsPayload("gpt-5.6-sol", "resp_1",
			`{"type":"function_call_output","call_id":"call_1","output":"done"}`))
	require.NoError(t, err)
	require.Len(t, upstream.bodies, 2)
	require.NotContains(t, string(upstream.bodies[1]), "collaboration",
		"the disabled group must keep the baseline behaviour and not rebuild tools")
}

// 缺少客户端原始来源时，OAuth pure bridge 同样在任何上游写入之前失败关闭。
func TestCodexV2BridgeOAuthUnsafeInheritedDeclarationFailsClosed(t *testing.T) {
	policy, err := NewCodexV2GroupPolicyFromBytes([]byte("version: 1\ngroups:\n  \"77\":\n    enabled: true\n"))
	require.NoError(t, err)
	restore := SetCurrentCodexV2GroupPolicyForTest(policy)
	t.Cleanup(restore)

	svc, upstream := newCodexV2BridgeTestService(t)
	c := newCodexV2BridgeTestContext(t, 77)

	// 只留下派生的上游声明，故意不提供客户端原始来源。
	setOpenAIWSHTTPBridgeToolState(c, openAIWSHTTPBridgeToolState{
		LoweredAdditionalTools: []byte(`[{"type":"additional_tools","tools":[{"type":"function","name":"shell"}]}]`),
	})

	payload := codexV2BridgeOmitToolsPayload("gpt-5.6-sol", "",
		`{"type":"function_call_output","call_id":"call_1","output":"done"}`)
	var frames []string
	_, turnErr := svc.proxyOpenAIWSHTTPBridgeTurn(
		c.Request.Context(), c, codexV2BridgeOAuthAccount(), "oauth-token",
		[]byte(payload), len(payload), "gpt-5.6-sol", "", "", "", "", 2,
		func(message []byte) error {
			frames = append(frames, string(message))
			return nil
		},
	)
	require.Error(t, turnErr)
	require.True(t, IsCodexV2GroupPolicyRejectionError(turnErr),
		"unsafe inheritance on the OAuth bridge must be a local group-policy rejection, got %v", turnErr)
	require.Empty(t, upstream.bodies, "fail-closed must not reach any upstream write")
	require.Len(t, frames, 1, "the client must receive exactly one explicit error frame")
	require.Contains(t, frames[0], codexV2UnsafeToolInheritanceMessage)
}
