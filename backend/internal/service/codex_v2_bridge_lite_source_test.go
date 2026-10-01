package service

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// 实际安装版 Codex 0.158 把 collaboration 声明放在 input[].additional_tools[].tools[]，
// 顶层 tools 为空。以下 fixture 复刻该载体；shell 用作“显式换成另一个声明”的对照。
const codexV2BridgeLiteCollaborationDeclaration = `{"type":"additional_tools","tools":[` +
	`{"type":"namespace","name":"collaboration","description":"sub-agents","tools":[` +
	`{"type":"function","name":"spawn_agent","parameters":{"type":"object","properties":{` +
	`"message":{"type":"string","encrypted":true},"fork_turns":{"type":"string"},"task_name":{"type":"string"}}}}]}]}`

const codexV2BridgeLiteShellDeclaration = `{"type":"additional_tools","tools":[` +
	`{"type":"function","name":"shell","parameters":{"type":"object","properties":{"command":{"type":"string"}}}}]}`

// codexV2BridgeLitePayload 组装一帧只带 Lite 载体（可选带空顶层 tools，模拟真实 CLI）。
func codexV2BridgeLitePayload(model, previousResponseID string, items ...string) string {
	return codexV2BridgeLitePayloadWithToolsField(model, previousResponseID, true, items...)
}

// codexV2BridgeLiteOnlyPayload 组装一帧完全不带顶层 tools 键的纯 Lite 载体请求；
// 复核者的复现路径就是这个形态（顶层无 tools）。
func codexV2BridgeLiteOnlyPayload(model, previousResponseID string, items ...string) string {
	return codexV2BridgeLitePayloadWithToolsField(model, previousResponseID, false, items...)
}

func codexV2BridgeLitePayloadWithToolsField(model, previousResponseID string, topLevelTools bool, items ...string) string {
	payload := `{"type":"response.create","model":"` + model + `","stream":true`
	if previousResponseID != "" {
		payload += `,"previous_response_id":"` + previousResponseID + `"`
	}
	if topLevelTools {
		payload += `,"tools":[]`
	}
	payload += `,"input":[`
	for i, item := range items {
		if i > 0 {
			payload += ","
		}
		payload += item
	}
	payload += `]}`
	return payload
}

func codexV2BridgeOmitToolsPayload(model, previousResponseID string, items ...string) string {
	payload := `{"type":"response.create","model":"` + model + `","stream":true`
	if previousResponseID != "" {
		payload += `,"previous_response_id":"` + previousResponseID + `"`
	}
	payload += `,"input":[`
	for i, item := range items {
		if i > 0 {
			payload += ","
		}
		payload += item
	}
	payload += `]}`
	return payload
}

func codexV2BridgeAdditionalToolsItem(body []byte) gjson.Result {
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return gjson.Result{}
	}
	var found gjson.Result
	input.ForEach(func(_, item gjson.Result) bool {
		if item.Get("type").String() == "additional_tools" {
			found = item
			return false
		}
		return true
	})
	return found
}

// 复核者复现的第一条路径：第一轮顶层 collaboration、第二轮 Lite 载体换 shell、第三轮
// 省略声明。第三轮必须重建第二轮声明的工具，既不能重放第一轮的陈旧 collaboration，
// 也不能让客户端声明静默消失。
func TestCodexV2BridgeTopLevelThenLiteReplacementThenOmit(t *testing.T) {
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
	account := codexV2BridgeOpenAIAccount()

	// 第 1 轮：顶层 collaboration 声明。
	_, err = codexV2BridgeRunTurn(t, svc, c, account, 1,
		`{"type":"response.create","model":"gpt-5.6-sol","stream":true,`+codexV2BridgeToolsDeclaration+`,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"spawn a helper"}]}]}`)
	require.NoError(t, err)
	state, ok := openAIWSHTTPBridgeToolStateFromContext(c)
	require.True(t, ok)
	require.True(t, state.CodexV2ParentPlaintextSchema)
	require.NotEmpty(t, gjson.GetBytes(state.LoweredTools, "#(name==\"sub2api_collaboration__spawn_agent\")").Raw,
		"premise: the parent turn derives flattened collaboration tools")

	// 第 2 轮：Lite 载体显式换成 shell（完全不带顶层 tools 键，复刻复核者路径）。
	_, err = codexV2BridgeRunTurn(t, svc, c, account, 2,
		codexV2BridgeLiteOnlyPayload("gpt-5.6-sol", "resp_1", codexV2BridgeLiteShellDeclaration,
			`{"type":"function_call_output","call_id":"call_1","output":"done"}`))
	require.NoError(t, err)
	require.Len(t, upstream.bodies, 2)
	second := upstream.bodies[1]
	require.NotContains(t, string(second), "collaboration__spawn_agent",
		"an explicit Lite replacement must drop the previous turn's declaration")
	require.Contains(t, string(second), `"name":"shell"`)
	state, ok = openAIWSHTTPBridgeToolStateFromContext(c)
	require.True(t, ok)
	require.False(t, codexV2ToolsDeclarationHasEntries(state.ClientTools),
		"the top-level client source must be replaced by the Lite declaration")
	require.NotEmpty(t, decodeOpenAIWSHTTPBridgeRawItems(state.ClientAdditionalTools),
		"the Lite client source must be recorded")
	require.False(t, state.CodexV2ParentPlaintextSchema,
		"the shell declaration carries no collaboration marker")

	// 第 3 轮：省略工具声明，必须重建第 2 轮的 Lite shell，而不是第一轮的 collaboration。
	thirdFrames, err := codexV2BridgeRunTurn(t, svc, c, account, 3,
		codexV2BridgeOmitToolsPayload("gpt-5.6-sol", "resp_2",
			`{"type":"function_call_output","call_id":"call_2","output":"done"}`))
	require.NoError(t, err)
	require.Len(t, upstream.bodies, 3)
	third := upstream.bodies[2]
	require.NotContains(t, string(third), "collaboration",
		"the stale first-turn declaration must not be replayed")
	require.Contains(t, string(third), `"name":"shell"`,
		"the client's current declaration must be restored")
	require.Equal(t, "shell", codexV2BridgeAdditionalToolsItem(third).Get("tools.0.name").String(),
		"the client declaration must not drift")
	require.Empty(t, codexV2BridgeMarkedFrames(thirdFrames),
		"no collaboration marker is expected for a shell-only declaration")
	state, ok = openAIWSHTTPBridgeToolStateFromContext(c)
	require.True(t, ok)
	loweredLiteItems := decodeOpenAIWSHTTPBridgeRawItems(state.LoweredAdditionalTools)
	require.Len(t, loweredLiteItems, 1, "the derived Lite declaration must stay in the connection state")
	require.Equal(t, "shell", gjson.GetBytes(loweredLiteItems[0], "tools.0.name").String())
	require.False(t, state.CodexV2ParentPlaintextSchema)
}

// 复核者复现的第二条路径：纯 Lite 首帧（从未有顶层 tools），第二轮省略声明。
// 修复前第二轮出站没有任何工具，客户端声明被静默丢弃。
func TestCodexV2BridgePureLiteFirstFrameThenOmit(t *testing.T) {
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

	firstPayload := codexV2BridgeLiteOnlyPayload("gpt-5.6-sol", "", codexV2BridgeLiteCollaborationDeclaration,
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"spawn a helper"}]}`)
	firstFrames, err := codexV2BridgeRunTurn(t, svc, c, account, 1, firstPayload)
	require.NoError(t, err)
	require.Len(t, upstream.bodies, 1)
	connector := codexV2BridgeAdditionalToolsItem(upstream.bodies[0])
	require.Equal(t, "sub2api_collaboration", connector.Get("tools.0.name").String())
	require.False(t, connector.Get("tools.0.tools.0.parameters.properties.message.encrypted").Exists(),
		"the parent declaration must be sent without the encrypted marker")
	require.NotEmpty(t, codexV2BridgeMarkedFrames(firstFrames))
	state, ok := openAIWSHTTPBridgeToolStateFromContext(c)
	require.True(t, ok)
	require.True(t, state.CodexV2ParentPlaintextSchema)
	require.NotEmpty(t, decodeOpenAIWSHTTPBridgeRawItems(state.ClientAdditionalTools),
		"a pure Lite first frame must be recorded as the client source")
	require.NotEmpty(t, decodeOpenAIWSHTTPBridgeRawItems(state.LoweredAdditionalTools),
		"the per-account derived Lite declaration must be recorded too")

	// 第二轮省略声明：必须重建同一个 Lite 声明并继续补 []。
	secondFrames, err := codexV2BridgeRunTurn(t, svc, c, account, 2,
		codexV2BridgeOmitToolsPayload("gpt-5.6-sol", "resp_1",
			`{"type":"function_call_output","call_id":"call_1","output":"done"}`))
	require.NoError(t, err)
	require.Len(t, upstream.bodies, 2)
	restored := codexV2BridgeAdditionalToolsItem(upstream.bodies[1])
	require.Equal(t, "sub2api_collaboration", restored.Get("tools.0.name").String(),
		"the client declaration must survive the omitted frame")
	require.False(t, restored.Get("tools.0.tools.0.parameters.properties.message.encrypted").Exists())
	require.NotEmpty(t, codexV2BridgeMarkedFrames(secondFrames),
		"the restored declaration must still produce encrypted_function_args")
}

// Lite ↔ Lite 显式替换：第三轮省略时必须使用第二轮的新声明。
func TestCodexV2BridgeLiteReplacementReplacesSource(t *testing.T) {
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
	account := codexV2BridgeOpenAIAccount()

	_, err = codexV2BridgeRunTurn(t, svc, c, account, 1,
		codexV2BridgeLitePayload("gpt-5.6-sol", "", codexV2BridgeLiteCollaborationDeclaration,
			`{"type":"message","role":"user","content":[{"type":"input_text","text":"spawn a helper"}]}`))
	require.NoError(t, err)
	_, err = codexV2BridgeRunTurn(t, svc, c, account, 2,
		codexV2BridgeLitePayload("gpt-5.6-sol", "resp_1", codexV2BridgeLiteShellDeclaration,
			`{"type":"function_call_output","call_id":"call_1","output":"done"}`))
	require.NoError(t, err)
	thirdFrames, err := codexV2BridgeRunTurn(t, svc, c, account, 3,
		codexV2BridgeOmitToolsPayload("gpt-5.6-sol", "resp_2",
			`{"type":"function_call_output","call_id":"call_2","output":"done"}`))
	require.NoError(t, err)
	require.Len(t, upstream.bodies, 3)
	require.NotContains(t, string(upstream.bodies[2]), "collaboration",
		"the replaced Lite declaration must not come back")
	require.Equal(t, "shell", codexV2BridgeAdditionalToolsItem(upstream.bodies[2]).Get("tools.0.name").String())
	require.Empty(t, codexV2BridgeMarkedFrames(thirdFrames))
}

// Lite → 顶层显式替换：第四轮省略时按顶层声明重建，回程标记回到 collaboration。
func TestCodexV2BridgeLiteToTopLevelReplacement(t *testing.T) {
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
	account := codexV2BridgeOpenAIAccount()

	_, err = codexV2BridgeRunTurn(t, svc, c, account, 1,
		codexV2BridgeLitePayload("gpt-5.6-sol", "", codexV2BridgeLiteCollaborationDeclaration,
			`{"type":"message","role":"user","content":[{"type":"input_text","text":"spawn a helper"}]}`))
	require.NoError(t, err)
	_, err = codexV2BridgeRunTurn(t, svc, c, account, 2,
		`{"type":"response.create","model":"gpt-5.6-sol","stream":true,"previous_response_id":"resp_1",`+
			codexV2BridgeToolsDeclaration+`,"input":[{"type":"function_call_output","call_id":"call_1","output":"done"}]}`)
	require.NoError(t, err)
	state, ok := openAIWSHTTPBridgeToolStateFromContext(c)
	require.True(t, ok)
	require.Empty(t, decodeOpenAIWSHTTPBridgeRawItems(state.ClientAdditionalTools),
		"an explicit top-level declaration replaces the Lite source")
	require.True(t, state.CodexV2ParentPlaintextSchema)

	thirdFrames, err := codexV2BridgeRunTurn(t, svc, c, account, 3,
		codexV2BridgeOmitToolsPayload("gpt-5.6-sol", "resp_2",
			`{"type":"function_call_output","call_id":"call_2","output":"done"}`))
	require.NoError(t, err)
	require.Len(t, upstream.bodies, 3)
	require.Contains(t, string(upstream.bodies[2]), "collaboration__spawn_agent",
		"the top-level client declaration must be restored")
	require.NotContains(t, string(upstream.bodies[2]), "additional_tools",
		"the replaced Lite declaration must not come back")
	require.NotEmpty(t, codexV2BridgeMarkedFrames(thirdFrames))
}

// 失败切账号：Lite 声明建状态后失败切到非 OpenAI 原生 Responses 子端，再切回父端省略
// 声明。子端必须收到客户端原样声明（保留 encrypted），父端必须重新去标记并补 []。
func TestCodexV2BridgeLiteFailoverRederivesPerAccount(t *testing.T) {
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

	_, err = codexV2BridgeRunTurn(t, svc, c, codexV2BridgeOpenAIAccount(), 1,
		codexV2BridgeLitePayload("gpt-5.6-sol", "", codexV2BridgeLiteCollaborationDeclaration,
			`{"type":"message","role":"user","content":[{"type":"input_text","text":"spawn a helper"}]}`))
	require.NoError(t, err)

	childFrames, err := codexV2BridgeRunTurn(t, svc, c, codexV2BridgeDeepSeekAccount(), 2,
		codexV2BridgeOmitToolsPayload("deepseek-flash", "resp_1",
			`{"type":"function_call_output","call_id":"call_1","output":"done"}`))
	require.NoError(t, err)
	require.Len(t, upstream.bodies, 2)
	childConnector := codexV2BridgeAdditionalToolsItem(upstream.bodies[1])
	require.Equal(t, "sub2api_collaboration", childConnector.Get("tools.0.name").String())
	require.False(t, childConnector.Get("tools.0.tools.0.parameters.properties.message.encrypted").Exists())
	require.NotEmpty(t, codexV2BridgeMarkedFrames(childFrames))
	state, ok := openAIWSHTTPBridgeToolStateFromContext(c)
	require.True(t, ok)
	require.True(t, state.CodexV2ParentPlaintextSchema,
		"the derived marker follows the actual alias declaration on every platform")

	parentFrames, err := codexV2BridgeRunTurn(t, svc, c, codexV2BridgeOpenAIAccount(), 3,
		codexV2BridgeOmitToolsPayload("gpt-5.6-sol", "resp_2",
			`{"type":"function_call_output","call_id":"call_2","output":"done"}`))
	require.NoError(t, err)
	require.Len(t, upstream.bodies, 3)
	parentConnector := codexV2BridgeAdditionalToolsItem(upstream.bodies[2])
	require.Equal(t, "sub2api_collaboration", parentConnector.Get("tools.0.name").String())
	require.False(t, parentConnector.Get("tools.0.tools.0.parameters.properties.message.encrypted").Exists(),
		"the parent attempt must strip the marker again")
	require.NotEmpty(t, codexV2BridgeMarkedFrames(parentFrames),
		"the parent attempt must restore encrypted_function_args")
	state, ok = openAIWSHTTPBridgeToolStateFromContext(c)
	require.True(t, ok)
	require.True(t, state.CodexV2ParentPlaintextSchema)
}

// 重连：新的 WS 连接不得继承上一连接的 Lite 声明。
func TestCodexV2BridgeLiteReconnectDoesNotInheritSource(t *testing.T) {
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
		codexV2BridgeLitePayload("gpt-5.6-sol", "", codexV2BridgeLiteCollaborationDeclaration,
			`{"type":"message","role":"user","content":[{"type":"input_text","text":"spawn a helper"}]}`))
	require.NoError(t, err)

	reconnected := newCodexV2BridgeTestContext(t, 77)
	frames, err := codexV2BridgeRunTurn(t, svc, reconnected, account, 1,
		codexV2BridgeOmitToolsPayload("gpt-5.6-sol", "",
			`{"type":"function_call_output","call_id":"call_1","output":"done"}`))
	require.NoError(t, err)
	require.Len(t, upstream.bodies, 2)
	require.NotContains(t, string(upstream.bodies[1]), "additional_tools",
		"a reconnected session must not inherit the previous connection's declaration")
	require.Empty(t, codexV2BridgeMarkedFrames(frames))
}

// 关闭组：Lite 载体照原样下发，省略声明时不重建来源（保持既有行为）。
func TestCodexV2BridgeLiteDisabledGroupUnchanged(t *testing.T) {
	policy, err := NewCodexV2GroupPolicyFromBytes([]byte("version: 1\ngroups:\n  \"77\":\n    enabled: true\n"))
	require.NoError(t, err)
	restore := SetCurrentCodexV2GroupPolicyForTest(policy)
	t.Cleanup(restore)

	svc, upstream := newCodexV2BridgeTestService(t,
		codexV2BridgeFunctionCallSSE("resp_1", "call_1"),
		codexV2BridgeFunctionCallSSE("resp_2", "call_2"),
	)
	c := newCodexV2BridgeTestContext(t, 88)
	account := codexV2BridgeOpenAIAccount()

	_, err = codexV2BridgeRunTurn(t, svc, c, account, 1,
		codexV2BridgeLitePayload("gpt-5.6-sol", "", codexV2BridgeLiteCollaborationDeclaration,
			`{"type":"message","role":"user","content":[{"type":"input_text","text":"spawn a helper"}]}`))
	require.NoError(t, err)
	require.Len(t, upstream.bodies, 1)
	require.True(t, codexV2BridgeAdditionalToolsItem(upstream.bodies[0]).
		Get("tools.0.tools.0.parameters.properties.message.encrypted").Exists(),
		"the disabled group must forward the client declaration untouched")

	_, err = codexV2BridgeRunTurn(t, svc, c, account, 2,
		codexV2BridgeOmitToolsPayload("gpt-5.6-sol", "resp_1",
			`{"type":"function_call_output","call_id":"call_1","output":"done"}`))
	require.NoError(t, err)
	require.Len(t, upstream.bodies, 2)
	require.NotContains(t, string(upstream.bodies[1]), "additional_tools",
		"the disabled group must keep the baseline behaviour and not restore tools")
}
