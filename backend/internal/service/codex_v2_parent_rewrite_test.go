package service

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const codexV2ParentToolsFixture = `{
  "model": "gpt-6-sol",
  "input": [
    {"type": "message", "role": "user", "content": [{"type": "input_text", "text": "spawn a helper"}]},
    {"type": "function_call", "call_id": "call_1", "name": "spawn_agent", "namespace": "collaboration", "arguments": "{\"fork_turns\":\"all\",\"message\":\"do the thing\"}"}
  ],
  "tools": [
    {
      "type": "namespace",
      "name": "collaboration",
      "description": "Tools for spawning and managing sub-agents.",
      "tools": [
        {"type": "function", "name": "spawn_agent", "parameters": {"type": "object", "properties": {"fork_turns": {"type": "string"}, "message": {"type": "string", "encrypted": true}, "task_name": {"type": "string"}}, "required": ["message"]}},
        {"type": "function", "name": "send_message", "parameters": {"type": "object", "properties": {"message": {"type": "string", "encrypted": true}, "target": {"type": "string"}}}},
        {"type": "function", "name": "followup_task", "parameters": {"type": "object", "properties": {"message": {"type": "string", "encrypted": true}, "target": {"type": "string"}}}},
        {"type": "function", "name": "wait_agent", "parameters": {"type": "object", "properties": {"timeout_ms": {"type": "number"}}}}
      ]
    },
    {"type": "function", "name": "shell", "parameters": {"type": "object", "properties": {"command": {"type": "string"}}}}
  ],
  "max_output_tokens": 9007199254740993
}`

func TestRewriteCodexV2ParentPlaintextToolSchema(t *testing.T) {
	t.Parallel()

	rewritten, applied, err := rewriteCodexV2ParentPlaintextToolSchema([]byte(codexV2ParentToolsFixture))
	require.NoError(t, err)
	require.True(t, applied)
	require.NotContains(t, string(rewritten), `"encrypted"`)
	require.Contains(t, string(rewritten), "9007199254740993", "unrelated numeric literal must be preserved")

	spawn := gjson.GetBytes(rewritten, "tools.0.tools.0")
	require.Equal(t, "function", spawn.Get("type").String())
	require.Equal(t, "object", spawn.Get("parameters.type").String())
	require.Equal(t, "string", spawn.Get("parameters.properties.message.type").String())
	require.False(t, spawn.Get("parameters.properties.message.encrypted").Exists())
	require.Equal(t, "string", spawn.Get("parameters.properties.fork_turns.type").String())
	require.Equal(t, "string", spawn.Get("parameters.properties.task_name.type").String())
	require.Equal(t, "number", gjson.GetBytes(rewritten, "tools.0.tools.3.parameters.properties.timeout_ms.type").String(),
		"wait_agent is not a message tool and must stay untouched")
	require.Equal(t, "shell", gjson.GetBytes(rewritten, "tools.1.name").String(),
		"top-level non-namespace tools must stay untouched")
	require.Equal(t, `{"fork_turns":"all","message":"do the thing"}`,
		gjson.GetBytes(rewritten, "input.1.arguments").String(),
		"historical call items must stay untouched")
	require.Equal(t, "Tools for spawning and managing sub-agents.",
		gjson.GetBytes(rewritten, "tools.0.description").String())
}

// 策略改写必须与既有的 namespace 摊平/还原链路可组合：改写在前，摊平复制的是
// 已移除 encrypted 标记的子工具声明；回程还原出 namespace/name 后再补
// encrypted_function_args。
func TestRewriteCodexV2ParentPlaintextToolSchemaComposesWithFlattening(t *testing.T) {
	t.Parallel()

	rewritten, applied, err := rewriteCodexV2ParentPlaintextToolSchema([]byte(codexV2ParentToolsFixture))
	require.NoError(t, err)
	require.True(t, applied)

	var request map[string]any
	require.NoError(t, json.Unmarshal(rewritten, &request))
	names, changed, err := apicompat.FlattenResponsesNamespaces(request)
	require.NoError(t, err)
	require.True(t, changed)
	flat, err := json.Marshal(request)
	require.NoError(t, err)
	require.NotContains(t, string(flat), `"encrypted"`)
	require.Equal(t, "string",
		gjson.GetBytes(flat, `tools.#(name=="sub2api_collaboration__spawn_agent").parameters.properties.message.type`).String())
	require.Contains(t, names, "sub2api_collaboration__spawn_agent")

	flatResponse := `{"type":"response.output_item.done","item":{"type":"function_call","call_id":"call_1","name":"sub2api_collaboration__spawn_agent","arguments":"{\"message\":\"plain task\"}"}}`
	restored, restoredChanged, err := apicompat.RestoreResponsesNamespaceCalls([]byte(flatResponse), names)
	require.NoError(t, err)
	require.True(t, restoredChanged)
	require.Equal(t, "sub2api_collaboration", gjson.GetBytes(restored, "item.namespace").String())
	require.Equal(t, "spawn_agent", gjson.GetBytes(restored, "item.name").String())

	marked := insertCodexV2PlaintextFunctionCallMarkers(restoreCodexV2NamespaceCalls(restored))
	require.Equal(t, "[]", gjson.GetBytes(marked, "item.encrypted_function_args").Raw)
}

func TestRewriteCodexV2ParentPlaintextToolSchemaLiteCarrier(t *testing.T) {
	t.Parallel()

	body := `{"model":"gpt-6-sol","input":[
	  {"type":"additional_tools","tools":[{"type":"namespace","name":"collaboration","tools":[
	    {"type":"function","name":"send_message","parameters":{"type":"object","properties":{"message":{"type":"string","encrypted":true},"target":{"type":"string"}}}}
	  ]}]},
	  {"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}
	],"tools":[{"type":"function","name":"shell","parameters":{"type":"object","properties":{"command":{"type":"string","encrypted":true}}}}]}`

	rewritten, applied, err := rewriteCodexV2ParentPlaintextToolSchema([]byte(body))
	require.NoError(t, err)
	require.True(t, applied)
	require.False(t, gjson.GetBytes(rewritten, "input.0.tools.0.tools.0.parameters.properties.message.encrypted").Exists())
	require.True(t, gjson.GetBytes(rewritten, "tools.0.parameters.properties.command.encrypted").Exists(),
		"the schema rewrite must not touch non-collaboration tools")
	require.Equal(t, "hi", gjson.GetBytes(rewritten, "input.1.content.0.text").String())
}

func TestRewriteCodexV2ParentPlaintextToolSchemaSkipsUnknownNamespaces(t *testing.T) {
	t.Parallel()

	body := `{"tools":[
	  {"type":"namespace","name":"my_collab","tools":[{"type":"function","name":"spawn_agent","parameters":{"type":"object","properties":{"message":{"type":"string","encrypted":true}}}}]},
	  {"type":"function","name":"spawn_agent","parameters":{"type":"object","properties":{"message":{"type":"string","encrypted":true}}}}
	]}`
	rewritten, applied, err := rewriteCodexV2ParentPlaintextToolSchema([]byte(body))
	require.NoError(t, err)
	require.False(t, applied, "custom namespaces and bare top-level tools must not be rewritten")
	require.Equal(t, body, string(rewritten))
}

func TestRewriteCodexV2ParentPlaintextToolSchemaNoopPaths(t *testing.T) {
	t.Parallel()

	noCollaboration := `{"model":"gpt-6-sol","tools":[{"type":"function","name":"shell"}]}`
	rewritten, applied, err := rewriteCodexV2ParentPlaintextToolSchema([]byte(noCollaboration))
	require.NoError(t, err)
	require.False(t, applied)
	require.Equal(t, noCollaboration, string(rewritten))

	notJSON := []byte(`{"tools":[{"type":"namespace","name":"collaboration"`)
	rewritten, applied, err = rewriteCodexV2ParentPlaintextToolSchema(notJSON)
	require.NoError(t, err)
	require.False(t, applied)
	require.Equal(t, string(notJSON), string(rewritten))
}

func TestInsertCodexV2PlaintextFunctionCallMarkers(t *testing.T) {
	t.Parallel()

	plainCall := `{"type":"function_call","id":"fc_1","call_id":"call_1","name":"spawn_agent","namespace":"collaboration","arguments":"{\"fork_turns\":\"all\",\"message\":\"run the tests\"}","internal_chat_message_metadata_passthrough":{"turn_id":"t1"}}`
	cipherCall := `{"type":"function_call","id":"fc_2","call_id":"call_2","name":"send_message","namespace":"collaboration","arguments":"{\"message\":\"gAAAAABq0mQ1eTk9S2x0UHNlY3JldA\",\"target\":\"/root/a\"}"}`
	longCipherCall := `{"type":"function_call","id":"fc_3","call_id":"call_3","name":"followup_task","namespace":"collaboration","arguments":"{\"message\":\"` + strings.Repeat("Ab3xQ9", 20) + `\",\"target\":\"/root/b\"}"}`
	otherToolCall := `{"type":"function_call","id":"fc_4","call_id":"call_4","name":"shell","arguments":"{\"message\":\"hello\"}"}`
	namespaceMismatch := `{"type":"function_call","id":"fc_5","call_id":"call_5","name":"send_message","namespace":"mcp__other","arguments":"{\"message\":\"hello\"}"}`
	flatNameCall := `{"type":"function_call","id":"fc_6","call_id":"call_6","name":"collaboration__followup_task","arguments":"{\"message\":\"plain enough\"}"}`
	noMessageCall := `{"type":"function_call","id":"fc_7","call_id":"call_7","name":"wait_agent","namespace":"collaboration","arguments":"{\"timeout_ms\":1}"}`
	bareNameCall := `{"type":"function_call","id":"fc_8","call_id":"call_8","name":"spawn_agent","arguments":"{\"message\":\"plain task\"}"}`

	tests := []struct {
		name       string
		payload    string
		markedPath string
		wantMarked bool
		wantRaw    string
	}{
		{
			name:       "non_streaming_output_item",
			payload:    `{"id":"resp_1","output":[` + plainCall + `],"usage":{"input_tokens":10},"numeric":9007199254740993}`,
			markedPath: "output.0.encrypted_function_args",
			wantMarked: true,
		},
		{
			name:       "sse_output_item_added",
			payload:    `{"type":"response.output_item.added","output_index":0,"item":` + plainCall + `}`,
			markedPath: "item.encrypted_function_args",
			wantMarked: true,
		},
		{
			name:       "sse_output_item_done",
			payload:    `{"type":"response.output_item.done","output_index":0,"item":` + plainCall + `}`,
			markedPath: "item.encrypted_function_args",
			wantMarked: true,
		},
		{
			name:       "terminal_response_completed",
			payload:    `{"type":"response.completed","response":{"id":"resp_1","output":[` + plainCall + `]}}`,
			markedPath: "response.output.0.encrypted_function_args",
			wantMarked: true,
		},
		{
			name:       "flat_name_without_namespace",
			payload:    `{"id":"resp_1","output":[` + flatNameCall + `]}`,
			markedPath: "output.0.encrypted_function_args",
			wantMarked: true,
		},
		{
			name:       "ciphertext_is_never_marked",
			payload:    `{"id":"resp_1","output":[` + cipherCall + `]}`,
			markedPath: "output.0.encrypted_function_args",
			wantMarked: false,
		},
		{
			name:       "long_base64_is_never_marked",
			payload:    `{"id":"resp_1","output":[` + longCipherCall + `]}`,
			markedPath: "output.0.encrypted_function_args",
			wantMarked: false,
		},
		{
			name:       "other_tool_is_untouched",
			payload:    `{"id":"resp_1","output":[` + otherToolCall + `]}`,
			markedPath: "output.0.encrypted_function_args",
			wantMarked: false,
		},
		{
			name:       "namespace_mismatch_is_untouched",
			payload:    `{"id":"resp_1","output":[` + namespaceMismatch + `]}`,
			markedPath: "output.0.encrypted_function_args",
			wantMarked: false,
		},
		{
			name:       "missing_message_argument_is_untouched",
			payload:    `{"id":"resp_1","output":[` + noMessageCall + `]}`,
			markedPath: "output.0.encrypted_function_args",
			wantMarked: false,
		},
		{
			name: "existing_encrypted_args_are_never_overwritten",
			payload: `{"id":"resp_1","output":[{"type":"function_call","id":"fc_8","call_id":"call_8","name":"send_message",` +
				`"namespace":"collaboration","arguments":"{\"message\":\"plain task\"}","encrypted_function_args":["message"]}]}`,
			markedPath: "output.0.encrypted_function_args",
			wantMarked: false,
			wantRaw:    `["message"]`,
		},
		{
			name:       "bare_name_without_namespace_is_not_collaboration",
			payload:    `{"id":"resp_1","output":[` + bareNameCall + `]}`,
			markedPath: "output.0.encrypted_function_args",
			wantMarked: false,
		},
		{
			name:       "delta_event_is_untouched",
			payload:    `{"type":"response.function_call_arguments.done","item_id":"fc_1","arguments":"{\"message\":\"run the tests\"}"}`,
			markedPath: "encrypted_function_args",
			wantMarked: false,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			marked := insertCodexV2PlaintextFunctionCallMarkers([]byte(tt.payload))
			require.True(t, gjson.ValidBytes(marked), "rewritten payload must stay valid JSON")
			result := gjson.GetBytes(marked, tt.markedPath)
			if tt.wantRaw != "" {
				require.True(t, result.Exists(), "expected %s in %s", tt.markedPath, string(marked))
				require.Equal(t, tt.wantRaw, result.Raw, "existing encrypted_function_args must stay untouched")
				return
			}
			if tt.wantMarked {
				require.True(t, result.Exists(), "expected %s in %s", tt.markedPath, string(marked))
				require.Equal(t, "[]", result.Raw, "encrypted_function_args must be an exact empty array")
			} else {
				require.False(t, result.Exists(), "did not expect %s in %s", tt.markedPath, string(marked))
			}
		})
	}
}

func TestInsertCodexV2PlaintextFunctionCallMarkersPreservesUnknownFields(t *testing.T) {
	t.Parallel()

	payload := `{"type":"response.output_item.done","item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"spawn_agent","namespace":"collaboration","arguments":"{\"message\":\"plain\"}","internal_chat_message_metadata_passthrough":{"turn_id":"turn_9","create_time":1759000000},"status":"completed"},"sequence_number":9007199254740993}`
	marked := insertCodexV2PlaintextFunctionCallMarkers([]byte(payload))
	require.Contains(t, string(marked), `"internal_chat_message_metadata_passthrough":{"turn_id":"turn_9","create_time":1759000000}`)
	require.Contains(t, string(marked), `"sequence_number":9007199254740993`)
	require.Contains(t, string(marked), `"status":"completed"`)
	require.Equal(t, "[]", gjson.GetBytes(marked, "item.encrypted_function_args").Raw)
}

// 顶层数组载荷不能生成带前导点的 gjson/sjson 路径，否则补标记会在写入时静默失败。
func TestInsertCodexV2PlaintextFunctionCallMarkersTopLevelArray(t *testing.T) {
	t.Parallel()

	payload := `[{"type":"function_call","id":"fc_1","call_id":"call_1","name":"collaboration__spawn_agent","arguments":"{\"message\":\"plain task\"}"}]`
	marked := insertCodexV2PlaintextFunctionCallMarkers([]byte(payload))
	require.True(t, gjson.ValidBytes(marked))
	require.Equal(t, "[]", gjson.GetBytes(marked, "0.encrypted_function_args").Raw,
		"a top-level array item must be addressed as 0.…, not .0.…")
	require.NotContains(t, string(marked), `".0.`)
}

func TestApplyCodexV2UpstreamRequestPoliciesAttemptAndFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)

	policy, err := NewCodexV2GroupPolicyFromBytes([]byte("version: 1\ngroups:\n  \"12\":\n    enabled: true\n"))
	require.NoError(t, err)
	restore := SetCurrentCodexV2GroupPolicyForTest(policy)
	t.Cleanup(restore)

	groupID := int64(12)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("api_key", &APIKey{ID: 1, GroupID: &groupID})

	openAIAccount := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	deepSeekAccount := &Account{ID: 2, Platform: PlatformDeepseek, Type: AccountTypeAPIKey}

	// 第一次尝试：OpenAI 上游，移除 encrypted 标记并登记本次尝试。
	policied, err := applyCodexV2UpstreamRequestPolicies(c, openAIAccount, []byte(codexV2ParentToolsFixture))
	require.NoError(t, err)
	require.NotContains(t, string(policied), `"encrypted"`)
	require.True(t, codexV2ParentPlaintextAttemptActive(c))

	response := `{"id":"resp_1","output":[{"type":"function_call","call_id":"call_1","name":"spawn_agent","namespace":"collaboration","arguments":"{\"message\":\"plain task\"}"}]}`
	require.Equal(t, "[]",
		gjson.GetBytes(applyCodexV2ParentResponsePolicy(c, []byte(response)), "output.0.encrypted_function_args").Raw)

	// 非 OpenAI 父端同样使用别名和明文；换成无协作声明的请求后才清空本轮标记。
	failoverBody, err := applyCodexV2UpstreamRequestPolicies(c, deepSeekAccount, []byte(codexV2ParentToolsFixture))
	require.NoError(t, err)
	require.NotContains(t, string(failoverBody), `"encrypted"`)
	require.True(t, codexV2ParentPlaintextAttemptActive(c))
	_, err = applyCodexV2UpstreamRequestPolicies(c, deepSeekAccount, []byte(`{"input":[]}`))
	require.NoError(t, err)
	require.False(t, codexV2ParentPlaintextAttemptActive(c))
	require.Equal(t, response, string(applyCodexV2ParentResponsePolicy(c, []byte(response))))
}

func TestApplyCodexV2UpstreamRequestPoliciesDisabledGroupUnchanged(t *testing.T) {
	gin.SetMode(gin.TestMode)

	policy, err := NewCodexV2GroupPolicyFromBytes([]byte("version: 1\ngroups:\n  \"12\":\n    enabled: false\n"))
	require.NoError(t, err)
	restore := SetCurrentCodexV2GroupPolicyForTest(policy)
	t.Cleanup(restore)

	groupID := int64(12)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("api_key", &APIKey{ID: 1, GroupID: &groupID})

	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	policied, err := applyCodexV2UpstreamRequestPolicies(c, account, []byte(codexV2ParentToolsFixture))
	require.NoError(t, err)
	require.Equal(t, codexV2ParentToolsFixture, string(policied), "disabled group must keep the request byte-identical")
	require.False(t, codexV2ParentPlaintextAttemptActive(c))

	response := `{"id":"resp_1","output":[{"type":"function_call","name":"spawn_agent","namespace":"collaboration","arguments":"{\"message\":\"plain\"}"}]}`
	require.Equal(t, response, string(applyCodexV2ParentResponsePolicy(c, []byte(response))))
}
