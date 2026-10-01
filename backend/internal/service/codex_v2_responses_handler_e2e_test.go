package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Codex V2 一期（按分组跨模型兼容）的 Responses 处理器级全链路用例。
//
// 四个入参都通过 OpenAIGatewayService.Forward 走真实请求构造与响应处理：
//   - 非流式 JSON：Forward -> handleNonStreamingResponse
//   - SSE：Forward -> handleStreamingResponseWithReasoning
//   - SSE→JSON：Forward -> handleNonStreamingResponse -> handleSSEToJSON
//   - 原生 passthrough：Forward -> forwardOpenAIPassthrough -> handle*ResponsePassthrough
//
// 假上游只替换 HTTPUpstream（不监听端口），因此不依赖本地 bind 权限；
// 断言的是上游实际收到的 schema 与客户端实际收到的报文，不直接调用改写纯函数。
const (
	codexV2E2EEnabledGroupID  int64 = 7301
	codexV2E2EDisabledGroupID int64 = 7302
	codexV2E2EModel                 = "gpt-6-sol"
	codexV2E2ECallID                = "call_parent_1"
	codexV2E2EItemID                = "fc_parent_1"
)

// codexV2E2EPolicyYAML 同时给出启用组（7301）与关闭组（7302）对照。
const codexV2E2EPolicyYAML = `version: 1
groups:
  "7301":
    enabled: true
  "7302":
    enabled: false
`

// codexV2E2EParentArguments 是父端（GPT）回程协作调用的明文 arguments。
const codexV2E2EParentArguments = `{"message":"run the tests","fork_turns":"none"}`

// codexV2E2EEncryptedMarkerPaths 是父端请求 fixture 里全部 encrypted 属性标记。
var codexV2E2EEncryptedMarkerPaths = []string{
	"tools.0.tools.0.parameters.properties.message.encrypted",
	"tools.0.tools.1.parameters.properties.message.encrypted",
	"tools.0.tools.2.parameters.properties.message.encrypted",
	"tools.1.parameters.properties.message.encrypted",
}

// codexV2E2EParentRequestBody 是父端 /v1/responses 请求 fixture：
// collaboration 命名空间的三个 message 工具带 encrypted 标记，其余声明必须原样保留
// （wait_agent 无 message、顶层 shell 的 message.encrypted 属于非协作工具）。
func codexV2E2EParentRequestBody(stream bool) []byte {
	return []byte(fmt.Sprintf(`{
  "model": %q,
  "stream": %t,
  "instructions": "spawn a helper",
  "input": [
    {"type":"message","role":"user","content":[{"type":"input_text","text":"spawn a helper"}]}
  ],
  "tools": [
    {
      "type": "namespace",
      "name": "collaboration",
      "description": "Tools for spawning and managing sub-agents.",
      "tools": [
        {"type":"function","name":"spawn_agent","parameters":{"type":"object","properties":{"fork_turns":{"type":"string"},"message":{"type":"string","description":"Task message","encrypted":true},"task_name":{"type":"string"}},"required":["message"]}},
        {"type":"function","name":"send_message","parameters":{"type":"object","properties":{"message":{"type":"string","encrypted":true},"target":{"type":"string"}}}},
        {"type":"function","name":"followup_task","parameters":{"type":"object","properties":{"message":{"type":"string","encrypted":true},"target":{"type":"string"}}}},
        {"type":"function","name":"wait_agent","parameters":{"type":"object","properties":{"timeout_ms":{"type":"number"}}}}
      ]
    },
    {"type":"function","name":"shell","parameters":{"type":"object","properties":{"command":{"type":"string"},"message":{"type":"string","encrypted":true}}}}
  ],
  "reasoning": {"effort": "medium"}
}`, codexV2E2EModel, stream))
}

// codexV2E2ECollaborationCallItem 是回程里 collaboration 命名空间的完整 function_call 项。
func codexV2E2ECollaborationCallItem(arguments string) string {
	return `{"type":"function_call","id":"` + codexV2E2EItemID + `","call_id":"` + codexV2E2ECallID +
		`","name":"spawn_agent","namespace":"collaboration","arguments":` + strconv.Quote(arguments) + `}`
}

// codexV2E2EFlatCollaborationCallItem 是声明被摊平后上游实际回传的形态：
// 名字带 collaboration__ 前缀、没有 namespace 字段。
func codexV2E2EFlatCollaborationCallItem(arguments string) string {
	return `{"type":"function_call","id":"` + codexV2E2EItemID + `","call_id":"` + codexV2E2ECallID +
		`","name":"` + codexV2CollaborationNamespace + `__spawn_agent","arguments":` + strconv.Quote(arguments) + `}`
}

// codexV2E2EParentLiteRequestBody 单独验证 additional_tools 声明载体：
// collaboration 工具挂在 input[].additional_tools[].tools[]，顶层 tools[] 只放非协作工具。
func codexV2E2EParentLiteRequestBody(stream bool) []byte {
	return []byte(fmt.Sprintf(`{
  "model": %q,
  "stream": %t,
  "input": [
    {"type":"additional_tools","tools":[{"type":"namespace","name":"collaboration","tools":[
      {"type":"function","name":"spawn_agent","parameters":{"type":"object","properties":{"message":{"type":"string","encrypted":true},"fork_turns":{"type":"string"}}}},
      {"type":"function","name":"send_message","parameters":{"type":"object","properties":{"message":{"type":"string","encrypted":true},"target":{"type":"string"}}}},
      {"type":"function","name":"followup_task","parameters":{"type":"object","properties":{"message":{"type":"string","encrypted":true},"target":{"type":"string"}}}},
      {"type":"function","name":"wait_agent","parameters":{"type":"object","properties":{"timeout_ms":{"type":"number"}}}}
    ]}]},
    {"type":"message","role":"user","content":[{"type":"input_text","text":"spawn a helper"}]}
  ],
  "tools": [
    {"type":"function","name":"shell","parameters":{"type":"object","properties":{"command":{"type":"string"},"message":{"type":"string","encrypted":true}}}}
  ]
}`, codexV2E2EModel, stream))
}

// codexV2E2EParentLiteOnlyRequestBody 复现本地探针记录的真实 Lite 请求形态：
// 顶层 tools 是空数组，唯一的 collaboration 声明在 input[].additional_tools[].tools[]，
// reasoning 只有 effort（context 由 Lite 归一化补），不含 parallel_tool_calls。
func codexV2E2EParentLiteOnlyRequestBody() []byte {
	return []byte(`{
  "model": "` + codexV2E2EModel + `",
  "stream": false,
  "reasoning": {"effort": "medium"},
  "input": [
    {"type":"additional_tools","role":"developer","tools":[{"type":"namespace","name":"collaboration","tools":[
      {"type":"function","name":"spawn_agent","parameters":{"type":"object","properties":{"message":{"type":"string","encrypted":true},"fork_turns":{"type":"string"}}}},
      {"type":"function","name":"send_message","parameters":{"type":"object","properties":{"message":{"type":"string","encrypted":true},"target":{"type":"string"}}}},
      {"type":"function","name":"followup_task","parameters":{"type":"object","properties":{"message":{"type":"string","encrypted":true},"target":{"type":"string"}}}},
      {"type":"function","name":"wait_agent","parameters":{"type":"object","properties":{"timeout_ms":{"type":"number"}}}}
    ]}]},
    {"type":"message","role":"user","content":[{"type":"input_text","text":"spawn a helper"}]}
  ],
  "tools": []
}`)
}

// codexV2E2EAssertLiteUpstreamSchemaRewritten 断言 Lite 请求经归一化后发给上游的最终 schema：
// 顶层 tools 仍为空数组，collaboration 三个 message 标记被去除，wait_agent 保持不变。
func codexV2E2EAssertLiteUpstreamSchemaRewritten(t *testing.T, sent []byte) {
	t.Helper()
	require.True(t, gjson.GetBytes(sent, "tools").IsArray(), "top-level tools must stay an array")
	require.Empty(t, gjson.GetBytes(sent, "tools").Array(), "the real Lite shape keeps top-level tools empty")
	for index, tool := range []string{"spawn_agent", "send_message", "followup_task"} {
		base := fmt.Sprintf("input.0.tools.0.tools.%d", index)
		require.Equal(t, tool, gjson.GetBytes(sent, base+".name").String(), "fixture order must stay stable")
		require.False(t, gjson.GetBytes(sent, base+".parameters.properties.message.encrypted").Exists(),
			"additional_tools %s.message.encrypted must be removed for the enabled group", tool)
	}
	require.Equal(t, "number",
		gjson.GetBytes(sent, "input.0.tools.0.tools.3.parameters.properties.timeout_ms.type").String(),
		"collaboration tools without message must stay untouched")
	require.Equal(t, "sub2api_collaboration", gjson.GetBytes(sent, "input.0.tools.0.name").String(),
		"the namespace declaration must stay in its carrier")
	require.Empty(t, codexV2E2EEncryptedPaths(sent),
		"the Lite-only fixture declares no other encrypted marker")
}

// codexV2E2EBareCollaborationNameCallItem 是裸名 send_message（没有 collaboration
// 命名空间）的完整调用项：不得被补 encrypted_function_args。
func codexV2E2EBareCollaborationNameCallItem() string {
	return `{"type":"function_call","id":"fc_bare_1","call_id":"call_bare_1","name":"send_message","arguments":` +
		strconv.Quote(codexV2E2EParentArguments) + `}`
}

// codexV2E2ENonMessageCollaborationCallItem 是 collaboration 下不带 message 的调用项：
// name 不是三个 message 工具，也不得被补标记。
func codexV2E2ENonMessageCollaborationCallItem() string {
	return `{"type":"function_call","id":"fc_wait_1","call_id":"call_wait_1","name":"wait_agent","namespace":"collaboration","arguments":"{\"timeout_ms\":1000}"}`
}

// codexV2E2EJSONResponseBody 是非流式 JSON 假上游响应，带一个不透明大整数用于验证定点写入。
func codexV2E2EJSONResponseBody() []byte {
	return []byte(`{"id":"resp_json_parent","object":"response","model":"` + codexV2E2EModel +
		`","status":"completed","metadata":{"opaque":9007199254740993},"output":[` +
		codexV2E2ECollaborationCallItem(codexV2E2EParentArguments) + `,` +
		codexV2E2EBareCollaborationNameCallItem() + `,` +
		codexV2E2ENonMessageCollaborationCallItem() +
		`],"usage":{"input_tokens":12,"output_tokens":7,"total_tokens":19}}`)
}

// codexV2E2ESSEDeltaChunks 是父端回程 arguments 的两个增量分片。
var codexV2E2ESSEDeltaChunks = []string{`{"message"`, `:"run the tests","fork_turns":"none"}`}

// codexV2E2ESSEStream 返回一段完整 SSE 假上游响应。
//
// singleShot=true 时 output_item.added 已带完整 arguments（加密参数一次性回传的形态）；
// singleShot=false 时模拟增量形态：added 的 arguments 为空，arguments 分片由 delta 事件给出。
func codexV2E2ESSEStream(singleShot bool) string {
	return codexV2E2ESSEStreamWithSpine(singleShot, codexV2E2ECollaborationCallItem, "response.completed")
}

// codexV2E2ESSEStreamWithSpine 与 codexV2E2ESSEStream 同构，只替换回程调用项的构造方式。
// terminalType 取 response.completed 或 response.done，两者在生产处理器里都是成功终态。
func codexV2E2ESSEStreamWithSpine(
	singleShot bool,
	collaborationItem func(arguments string) string,
	terminalType string,
) string {
	fullItem := collaborationItem(codexV2E2EParentArguments)
	addedItem := fullItem
	if !singleShot {
		addedItem = collaborationItem("")
	}
	frames := []string{
		`event: response.created` + "\n" +
			`data: {"type":"response.created","response":{"id":"resp_sse_parent","model":"` + codexV2E2EModel + `","status":"in_progress","output":[]}}`,
		`event: response.output_item.added` + "\n" +
			`data: {"type":"response.output_item.added","output_index":0,"item":` + addedItem + `}`,
	}
	for _, chunk := range codexV2E2ESSEDeltaChunks {
		frames = append(frames,
			`event: response.function_call_arguments.delta`+"\n"+
				`data: {"type":"response.function_call_arguments.delta","item_id":"`+codexV2E2EItemID+
				`","output_index":0,"delta":`+strconv.Quote(chunk)+`}`)
	}
	frames = append(frames,
		`event: response.output_item.done`+"\n"+
			`data: {"type":"response.output_item.done","output_index":0,"item":`+fullItem+`}`,
		`event: `+terminalType+"\n"+
			`data: {"type":"`+terminalType+`","response":{"id":"resp_sse_parent","model":"`+codexV2E2EModel+
			`","status":"completed","output":[`+fullItem+`,`+codexV2E2EBareCollaborationNameCallItem()+`,`+
			codexV2E2ENonMessageCollaborationCallItem()+
			`],"usage":{"input_tokens":12,"output_tokens":7,"total_tokens":19}}}`)
	return strings.Join(frames, "\n\n") + "\n\n"
}

// ── 测试脚手架 ──

// codexV2E2EInstallPolicy 安装启用组/关闭组策略，测试结束恢复进程级快照。
func codexV2E2EInstallPolicy(t *testing.T) {
	t.Helper()
	policy, err := NewCodexV2GroupPolicyFromBytes([]byte(codexV2E2EPolicyYAML))
	require.NoError(t, err)
	require.True(t, policy.EnabledForGroupID(codexV2E2EEnabledGroupID))
	require.False(t, policy.EnabledForGroupID(codexV2E2EDisabledGroupID))
	restore := SetCurrentCodexV2GroupPolicyForTest(policy)
	t.Cleanup(restore)
}

// codexV2E2ENewService 构造只替换 HTTPUpstream 的真实 OpenAIGatewayService。
func codexV2E2ENewService(resp *http.Response) (*OpenAIGatewayService, *httpUpstreamRecorder) {
	upstream := &httpUpstreamRecorder{resp: resp}
	cfg := &config.Config{}
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	return &OpenAIGatewayService{
		cfg:              cfg,
		httpUpstream:     upstream,
		cache:            &stubGatewayCache{},
		openaiWSResolver: NewOpenAIWSProtocolResolver(cfg),
		toolCorrector:    NewCodexToolCorrector(),
	}, upstream
}

// codexV2E2ENewContext 构造带 API Key 分组（即策略判定来源）的客户端上下文。
func codexV2E2ENewContext(groupID int64) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set("User-Agent", "unit-test-agent/1.0")
	// Accept 只在 passthrough 的请求头白名单里；非透传路径不得把它转给上游，
	// 用它区分 Forward 实际进入的上游请求构造分支。
	c.Request.Header.Set("Accept", "text/event-stream")
	id := groupID
	c.Set("api_key", &APIKey{ID: 7310, GroupID: &id})
	return c, recorder
}

// codexV2E2ENewLiteContext 构造真实 Lite 客户端的上下文：除分组外还带
// X-OpenAI-Internal-Codex-Responses-Lite: true，且显式声明 HTTP 入站传输。
func codexV2E2ENewLiteContext(groupID int64) (*gin.Context, *httptest.ResponseRecorder) {
	c, recorder := codexV2E2ENewContext(groupID)
	c.Request.Header.Set(responsesLiteHeader, "true")
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
	return c, recorder
}

// codexV2E2EAssertUpstreamCalledOnce 断言假上游在传输边界恰好被调用一次。
// 只看 lastBody 会被“先发一次未改写请求、再重试一次改写请求”掩盖，因此必须
// 同时钉住 requests/bodies 的数量。
func codexV2E2EAssertUpstreamCalledOnce(t *testing.T, upstream *httpUpstreamRecorder) {
	t.Helper()
	require.Len(t, upstream.requests, 1, "the fake upstream must be called exactly once")
	require.Len(t, upstream.bodies, 1, "the fake upstream must receive exactly one body")
}

// codexV2E2EAssertUpstreamNotCalled 断言本地拒绝没有触发任何上游调用。
func codexV2E2EAssertUpstreamNotCalled(t *testing.T, upstream *httpUpstreamRecorder) {
	t.Helper()
	require.Empty(t, upstream.requests, "a local rejection must not reach the upstream")
	require.Empty(t, upstream.bodies, "a local rejection must not send any upstream body")
}

// codexV2E2EAssertNonPassthroughRequest 断言走的是非透传上游请求构造。
func codexV2E2EAssertNonPassthroughRequest(t *testing.T, c *gin.Context, req *http.Request) {
	t.Helper()
	require.NotNil(t, req)
	_, branch := c.Get("openai_passthrough")
	require.False(t, branch, "a non-passthrough account must not enter forwardOpenAIPassthrough")
	require.Empty(t, req.Header.Get("Accept"),
		"the non-passthrough builder must not forward the client Accept header")
}

// codexV2E2EAssertPassthroughRequest 断言走的是原生透传上游请求构造。
func codexV2E2EAssertPassthroughRequest(t *testing.T, c *gin.Context, req *http.Request) {
	t.Helper()
	require.NotNil(t, req)
	branch, exists := c.Get("openai_passthrough")
	require.True(t, exists, "the passthrough branch marks the request context")
	require.Equal(t, true, branch)
	require.Equal(t, "text/event-stream", req.Header.Get("Accept"),
		"the passthrough builder must forward the client Accept header")
}

// codexV2E2EParentAccount 是 OpenAI 平台 API Key 账号（原生 Responses 父端）。
func codexV2E2EParentAccount(passthrough bool) *Account {
	extra := map[string]any{}
	if passthrough {
		extra["openai_passthrough"] = true
	}
	return &Account{
		ID: 9201, Name: "codex-v2-e2e-parent", Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Status: StatusActive, Schedulable: true, Concurrency: 1,
		Credentials: map[string]any{"api_key": "sk-upstream-parent"},
		Extra:       extra,
	}
}

// codexV2E2EParentOAuthAccount 是 Codex 官方客户端实际使用的 OAuth 父端账号。
// flattenNamespaces 控制是否开启 namespace 摊平：开启用于验证「先改写、后摊平、
// 回程还原再补 []」的组合，关闭用于真实 Lite 组合。
func codexV2E2EParentOAuthAccount(flattenNamespaces bool) *Account {
	extra := map[string]any{}
	if flattenNamespaces {
		extra["openai_responses_flatten_namespaces"] = true
	}
	return &Account{
		ID: 9202, Name: "codex-v2-e2e-oauth", Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: true, Concurrency: 1,
		Credentials: map[string]any{"access_token": "oauth-access-token"},
		Extra:       extra,
	}
}

// codexV2E2EChildAccount 是启用组里的非 OpenAI 平台子端账号（无原生 Responses 需求，
// 只用于本地拒绝用例：策略在任何上游写入之前就必须失败关闭）。
func codexV2E2EChildAccount() *Account {
	return &Account{
		ID: 9203, Name: "codex-v2-e2e-child", Platform: PlatformDeepseek, Type: AccountTypeAPIKey,
		Status: StatusActive, Schedulable: true, Concurrency: 1,
		Credentials: map[string]any{"api_key": "sk-upstream-child"},
	}
}

// codexV2E2EEncryptedPaths 收集 body 里所有值为 true 的 encrypted 属性路径。
func codexV2E2EEncryptedPaths(body []byte) []string {
	var paths []string
	var walk func(node gjson.Result, prefix string)
	walk = func(node gjson.Result, prefix string) {
		if node.IsArray() {
			index := 0
			node.ForEach(func(_, child gjson.Result) bool {
				walk(child, fmt.Sprintf("%s.%d", prefix, index))
				index++
				return true
			})
			return
		}
		if !node.IsObject() {
			return
		}
		node.ForEach(func(key, child gjson.Result) bool {
			path := key.String()
			if prefix != "" {
				path = prefix + "." + path
			}
			if key.String() == "encrypted" && child.Type == gjson.True {
				paths = append(paths, path)
			}
			walk(child, path)
			return true
		})
	}
	walk(gjson.ParseBytes(body), "")
	return paths
}

// codexV2E2EAssertUpstreamSchemaRewritten 断言假上游收到的父端 schema 只改了目标三处。
func codexV2E2EAssertUpstreamSchemaRewritten(t *testing.T, sent []byte) {
	t.Helper()
	for index, tool := range []string{"spawn_agent", "send_message", "followup_task"} {
		base := fmt.Sprintf("tools.0.tools.%d", index)
		require.Equal(t, tool, gjson.GetBytes(sent, base+".name").String(), "fixture order must stay stable")
		require.Equal(t, "string", gjson.GetBytes(sent, base+".parameters.properties.message.type").String())
		require.False(t, gjson.GetBytes(sent, base+".parameters.properties.message.encrypted").Exists(),
			"collaboration %s.message.encrypted must be removed for the enabled group", tool)
	}
	require.Equal(t, "Task message",
		gjson.GetBytes(sent, "tools.0.tools.0.parameters.properties.message.description").String(),
		"sibling properties must survive the marker removal")
	require.Equal(t, "message",
		gjson.GetBytes(sent, "tools.0.tools.0.parameters.required.0").String(),
		"required lists must stay untouched")
	require.Equal(t, "number",
		gjson.GetBytes(sent, "tools.0.tools.3.parameters.properties.timeout_ms.type").String(),
		"collaboration tools without message must stay untouched")
	require.Equal(t, "Tools for spawning and managing sub-agents.",
		gjson.GetBytes(sent, "tools.0.description").String())
	require.True(t, gjson.GetBytes(sent, "tools.1.parameters.properties.message.encrypted").Bool(),
		"non-collaboration tools must keep their encrypted marker")
	require.Equal(t, []string{"tools.1.parameters.properties.message.encrypted"}, codexV2E2EEncryptedPaths(sent),
		"exactly the three collaboration message markers may be removed")
}

// codexV2E2EAssertClientParentCallMarked 断言客户端收到的完整 collaboration 调用项。
func codexV2E2EAssertClientParentCallMarked(t *testing.T, body []byte, prefix string) {
	t.Helper()
	item := gjson.GetBytes(body, prefix)
	require.True(t, item.Exists(), "client payload must carry the collaboration call at %s", prefix)
	require.Equal(t, "function_call", item.Get("type").String())
	require.Equal(t, "collaboration", item.Get("namespace").String(), "original namespace must survive")
	require.Equal(t, "spawn_agent", item.Get("name").String())
	require.Equal(t, codexV2E2EParentArguments, item.Get("arguments").String(), "arguments must stay byte-identical")
	require.Equal(t, "[]", item.Get("encrypted_function_args").Raw,
		"the complete collaboration call must carry the exact empty encryption marker set")
}

// codexV2E2EAssertClientUnmarkedCallsUntouched 断言非协作/非 message 调用项不被补标记。
func codexV2E2EAssertClientUnmarkedCallsUntouched(t *testing.T, body []byte, prefixes ...string) {
	t.Helper()
	for _, prefix := range prefixes {
		item := gjson.GetBytes(body, prefix)
		require.True(t, item.Exists(), "fixture call at %s must reach the client", prefix)
		require.False(t, item.Get("encrypted_function_args").Exists(),
			"only complete collaboration message calls may be marked, got %s", prefix)
	}
}

// codexV2E2ESSEFrame 是解析后的客户端 SSE 帧。
type codexV2E2ESSEFrame struct {
	eventType string
	data      string
}

// codexV2E2EParseSSEFrames 解析客户端实际收到的 SSE body。
func codexV2E2EParseSSEFrames(body string) []codexV2E2ESSEFrame {
	var frames []codexV2E2ESSEFrame
	pendingEvent := ""
	for _, rawLine := range strings.Split(body, "\n") {
		line := strings.TrimRight(rawLine, "\r")
		if strings.HasPrefix(line, "event:") {
			pendingEvent = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			if strings.TrimSpace(line) == "" {
				pendingEvent = ""
			}
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		eventType := strings.TrimSpace(pendingEvent)
		if eventType == "" && data != "[DONE]" {
			eventType = strings.TrimSpace(gjson.Get(data, "type").String())
		}
		frames = append(frames, codexV2E2ESSEFrame{eventType: eventType, data: data})
		pendingEvent = ""
	}
	return frames
}

// codexV2E2EFramesOfType 返回指定事件类型的帧。
func codexV2E2EFramesOfType(frames []codexV2E2ESSEFrame, eventType string) []codexV2E2ESSEFrame {
	var matched []codexV2E2ESSEFrame
	for _, frame := range frames {
		if frame.eventType == eventType {
			matched = append(matched, frame)
		}
	}
	return matched
}

// codexV2E2EAssertStreamedClientPayload 断言客户端收到的 SSE 事件序列：
// added/done/completed 的完整项带精确 []，delta 分片逐字节保留且从不被注入标记。
//
// singleShot=true 时 output_item.added 已携带完整 arguments（加密参数一次回传的形态）；
// singleShot=false 时 added 的参数不完整，只有 done/completed 才允许补标记。
// terminalType 允许复用同一套断言覆盖 response.completed 与 response.done。
func codexV2E2EAssertStreamedClientPayload(t *testing.T, stream string, singleShot bool, terminalType string) {
	t.Helper()
	frames := codexV2E2EParseSSEFrames(stream)
	var order []string
	for _, frame := range frames {
		order = append(order, frame.eventType)
	}
	require.Equal(t, []string{
		"response.created",
		"response.output_item.added",
		"response.function_call_arguments.delta",
		"response.function_call_arguments.delta",
		"response.output_item.done",
		terminalType,
	}, order, "the streamed event order and delta boundaries must stay intact")

	added := codexV2E2EFramesOfType(frames, "response.output_item.added")
	require.Len(t, added, 1)
	if singleShot {
		codexV2E2EAssertClientParentCallMarked(t, []byte(added[0].data), "item")
	} else {
		require.Equal(t, "", gjson.Get(added[0].data, "item.arguments").String(),
			"an incomplete item must not be marked before its arguments are complete")
		require.False(t, gjson.Get(added[0].data, "item.encrypted_function_args").Exists())
	}

	// delta 边界：分片逐字节保留，且绝不能被注入标记。
	deltas := codexV2E2EFramesOfType(frames, "response.function_call_arguments.delta")
	require.Len(t, deltas, len(codexV2E2ESSEDeltaChunks))
	var joined strings.Builder
	for index, delta := range deltas {
		require.Equal(t, codexV2E2ESSEDeltaChunks[index], gjson.Get(delta.data, "delta").String(),
			"delta chunks must keep their exact boundaries")
		require.False(t, gjson.Get(delta.data, "encrypted_function_args").Exists())
		joined.WriteString(gjson.Get(delta.data, "delta").String())
	}
	require.Equal(t, codexV2E2EParentArguments, joined.String())

	done := codexV2E2EFramesOfType(frames, "response.output_item.done")
	require.Len(t, done, 1)
	codexV2E2EAssertClientParentCallMarked(t, []byte(done[0].data), "item")

	terminal := codexV2E2EFramesOfType(frames, terminalType)
	require.Len(t, terminal, 1)
	codexV2E2EAssertClientParentCallMarked(t, []byte(terminal[0].data), "response.output.0")
	codexV2E2EAssertClientUnmarkedCallsUntouched(t, []byte(terminal[0].data), "response.output.1", "response.output.2")
	require.Equal(t, int64(19), gjson.Get(terminal[0].data, "response.usage.total_tokens").Int())
}

// codexV2E2ECallSSEWithTerminal 用给定完整 function_call 项拼一段含
// output_item.added / output_item.done / terminalType 的 SSE 回程。
func codexV2E2ECallSSEWithTerminal(item, terminalType, responseID string) string {
	frames := []string{
		`event: response.output_item.added` + "\n" +
			`data: {"type":"response.output_item.added","output_index":0,"item":` + item + `}`,
		`event: response.output_item.done` + "\n" +
			`data: {"type":"response.output_item.done","output_index":0,"item":` + item + `}`,
		`event: ` + terminalType + "\n" +
			`data: {"type":"` + terminalType + `","response":{"id":"` + responseID + `","model":"` + codexV2E2EModel +
			`","status":"completed","output":[` + item +
			`],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`,
	}
	return strings.Join(frames, "\n\n") + "\n\n"
}

// codexV2E2EAssertClientCallPreservedEverywhere 断言 added / done / 终态三处携带同一个
// 完整调用项，且加密参数集合保持给定形态：
// wantMarker="" 表示三处都不得出现 encrypted_function_args；
// wantMarker 非空时表示三处都必须逐字保留该原始值。
func codexV2E2EAssertClientCallPreservedEverywhere(
	t *testing.T,
	stream string,
	item string,
	terminalType string,
	wantMarker string,
) {
	t.Helper()
	frames := codexV2E2EParseSSEFrames(stream)
	type frameCase struct {
		eventType string
		path      string
	}
	cases := []frameCase{
		{eventType: "response.output_item.added", path: "item"},
		{eventType: "response.output_item.done", path: "item"},
		{eventType: terminalType, path: "response.output.0"},
	}
	for _, tc := range cases {
		matched := codexV2E2EFramesOfType(frames, tc.eventType)
		require.Len(t, matched, 1, "exactly one %s frame must reach the client", tc.eventType)
		raw := gjson.Get(matched[0].data, tc.path).Raw
		require.NotEmpty(t, raw, "%s must carry the complete call item", tc.eventType)
		require.JSONEq(t, item, raw,
			"%s must preserve every field of the complete call item", tc.eventType)
		require.Equal(t, gjson.Get(item, "arguments").String(), gjson.Get(raw, "arguments").String(),
			"%s must preserve the arguments string byte-for-byte", tc.eventType)
		marker := gjson.Get(matched[0].data, tc.path+".encrypted_function_args")
		if wantMarker == "" {
			require.False(t, marker.Exists(), "%s must not gain an encrypted_function_args marker", tc.eventType)
			continue
		}
		require.Equal(t, wantMarker, marker.Raw,
			"%s must keep the upstream encrypted_function_args value", tc.eventType)
	}
	if wantMarker == "" {
		require.Equal(t, 0, strings.Count(stream, "encrypted_function_args"),
			"no frame may carry an encrypted_function_args marker")
	}
}

// codexV2E2EUpstreamResponse 构造假上游响应。
func codexV2E2EUpstreamResponse(contentType, body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{contentType}, "X-Request-Id": []string{"req_e2e"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// ── 路径 1：非流式 JSON ──

func TestCodexV2ResponsesHTTPJSONParentPolicyE2E(t *testing.T) {
	codexV2E2EInstallPolicy(t)
	response := codexV2E2EJSONResponseBody()
	svc, upstream := codexV2E2ENewService(codexV2E2EUpstreamResponse("application/json", string(response)))
	c, recorder := codexV2E2ENewContext(codexV2E2EEnabledGroupID)
	account := codexV2E2EParentAccount(false)
	body := codexV2E2EParentRequestBody(false)

	result, err := svc.Forward(context.Background(), c, account, body)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "application/json", recorder.Header().Get("Content-Type"),
		"the non-SSE JSON path must relay the upstream response as-is")
	codexV2E2EAssertUpstreamCalledOnce(t, upstream)
	codexV2E2EAssertNonPassthroughRequest(t, c, upstream.lastReq)
	require.Equal(t, "api.openai.com", upstream.lastReq.URL.Host)
	require.Equal(t, "/v1/responses", upstream.lastReq.URL.Path)

	codexV2E2EAssertUpstreamSchemaRewritten(t, upstream.lastBody)

	clientBody := recorder.Body.Bytes()
	codexV2E2EAssertClientParentCallMarked(t, clientBody, "output.0")
	codexV2E2EAssertClientUnmarkedCallsUntouched(t, clientBody, "output.1", "output.2")
	require.Equal(t, 1, strings.Count(string(clientBody), "encrypted_function_args"),
		"only the collaboration message call may be marked")
	require.Equal(t, "9007199254740993", gjson.GetBytes(clientBody, "metadata.opaque").Raw,
		"opaque numeric literals must survive the point write")
	require.Equal(t, int64(12), gjson.GetBytes(clientBody, "usage.input_tokens").Int())
}

// 本用例刻意只验证「additional_tools 载体上的改写」：不带 Lite 请求头，
// 因此 normalizeOpenAIResponsesLitePayloadForAccount 不执行，顶层 tools 里还留着
// 非协作工具。真实 Lite 组合（带请求头、顶层 tools 为空）见
// TestCodexV2ResponsesLiteHeaderNormalizationE2E，两者不互相替代。
func TestCodexV2ResponsesLiteAdditionalToolsCarrierE2E(t *testing.T) {
	codexV2E2EInstallPolicy(t)
	response := codexV2E2EJSONResponseBody()
	svc, upstream := codexV2E2ENewService(codexV2E2EUpstreamResponse("application/json", string(response)))
	c, recorder := codexV2E2ENewContext(codexV2E2EEnabledGroupID)
	account := codexV2E2EParentAccount(false)

	_, err := svc.Forward(context.Background(), c, account, codexV2E2EParentLiteRequestBody(false))

	require.NoError(t, err)
	codexV2E2EAssertUpstreamCalledOnce(t, upstream)
	codexV2E2EAssertNonPassthroughRequest(t, c, upstream.lastReq)
	require.False(t, isOpenAIResponsesLiteHeader(c.GetHeader(responsesLiteHeader)),
		"this case must stay the non-Lite carrier form")
	for index, tool := range []string{"spawn_agent", "send_message", "followup_task"} {
		base := fmt.Sprintf("input.0.tools.0.tools.%d", index)
		require.Equal(t, tool, gjson.GetBytes(upstream.lastBody, base+".name").String())
		require.False(t, gjson.GetBytes(upstream.lastBody, base+".parameters.properties.message.encrypted").Exists(),
			"additional_tools %s.message.encrypted must be removed", tool)
	}
	require.Equal(t, "number",
		gjson.GetBytes(upstream.lastBody, "input.0.tools.0.tools.3.parameters.properties.timeout_ms.type").String())
	require.True(t, gjson.GetBytes(upstream.lastBody, "tools.0.parameters.properties.message.encrypted").Bool(),
		"top-level non-collaboration tools must stay untouched")
	require.Equal(t, []string{"tools.0.parameters.properties.message.encrypted"},
		codexV2E2EEncryptedPaths(upstream.lastBody))

	clientBody := recorder.Body.Bytes()
	codexV2E2EAssertClientParentCallMarked(t, clientBody, "output.0")
	codexV2E2EAssertClientUnmarkedCallsUntouched(t, clientBody, "output.1", "output.2")
}

// 真实 Lite 组合：带 X-OpenAI-Internal-Codex-Responses-Lite: true，顶层 tools 为空数组，
// collaboration 声明只存在于 input[].additional_tools[].tools[]。
//
// 这里必须用 OAuth 父端账号：normalizeOpenAIResponsesLitePayloadForAccount 对
// OAuth-like 账号走 normalizeOpenAIResponsesLiteToolsPayload（完整的 Lite 工具契约），
// 对 API Key 账号只走 parallel_tool_calls 变体。上游 body 里出现
// reasoning.context=all_turns 与 parallel_tool_calls=false，是归一化确实执行过的证据。
func TestCodexV2ResponsesLiteHeaderNormalizationE2E(t *testing.T) {
	codexV2E2EInstallPolicy(t)
	response := codexV2E2EJSONResponseBody()
	svc, upstream := codexV2E2ENewService(codexV2E2EUpstreamResponse("application/json", string(response)))
	c, recorder := codexV2E2ENewLiteContext(codexV2E2EEnabledGroupID)
	account := codexV2E2EParentOAuthAccount(false)

	result, err := svc.Forward(context.Background(), c, account, codexV2E2EParentLiteOnlyRequestBody())

	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, isOpenAIResponsesLiteHeader(c.GetHeader(responsesLiteHeader)),
		"the request must really carry the Lite header")
	codexV2E2EAssertUpstreamCalledOnce(t, upstream)
	require.NotNil(t, upstream.lastReq)
	require.Equal(t, "chatgpt.com", upstream.lastReq.URL.Host)
	require.Equal(t, "/backend-api/codex/responses", upstream.lastReq.URL.Path)

	// Lite 归一化必须实际执行：reasoning.context 被补成 all_turns、parallel_tool_calls
	// 被显式写成 false，且原本的 reasoning.effort 保留。
	require.Equal(t, "all_turns", gjson.GetBytes(upstream.lastBody, "reasoning.context").String(),
		"normalizeOpenAIResponsesLitePayloadForAccount must have run on this request")
	require.Equal(t, "medium", gjson.GetBytes(upstream.lastBody, "reasoning.effort").String())
	require.False(t, gjson.GetBytes(upstream.lastBody, "parallel_tool_calls").Bool())
	require.True(t, gjson.GetBytes(upstream.lastBody, "parallel_tool_calls").Exists(),
		"the Lite contract requires an explicit parallel_tool_calls value")

	// 上游最终 schema：标记只在 additional_tools 载体上，且只有三个 message 工具被改。
	codexV2E2EAssertLiteUpstreamSchemaRewritten(t, upstream.lastBody)

	// 回程：客户端仍拿到原 namespace 与精确 []。
	clientBody := recorder.Body.Bytes()
	codexV2E2EAssertClientParentCallMarked(t, clientBody, "output.0")
	codexV2E2EAssertClientUnmarkedCallsUntouched(t, clientBody, "output.1", "output.2")
	require.Equal(t, 1, strings.Count(string(clientBody), "encrypted_function_args"))
}

// Lite 归一化的另一半契约：顶层 namespace 工具必须被搬进 input[].additional_tools，
// 且搬运之后的声明仍要被去标记（Lite 归一化先搬运，分组策略随后去除标记）。
func TestCodexV2ResponsesLiteNamespaceMigrationE2E(t *testing.T) {
	codexV2E2EInstallPolicy(t)
	response := codexV2E2EJSONResponseBody()
	svc, upstream := codexV2E2ENewService(codexV2E2EUpstreamResponse("application/json", string(response)))
	c, recorder := codexV2E2ENewLiteContext(codexV2E2EEnabledGroupID)
	account := codexV2E2EParentOAuthAccount(false)

	_, err := svc.Forward(context.Background(), c, account, codexV2E2EParentRequestBody(false))

	require.NoError(t, err)
	require.NotNil(t, upstream.lastReq)
	codexV2E2EAssertUpstreamCalledOnce(t, upstream)
	require.False(t, gjson.GetBytes(upstream.lastBody, `tools.#(name=="collaboration")`).Exists(),
		"the Lite contract requires namespace tools to move out of top-level tools")
	require.True(t, gjson.GetBytes(upstream.lastBody, `tools.#(name=="shell")`).Exists(),
		"non-namespace top-level tools must stay where they are")
	require.True(t, gjson.GetBytes(upstream.lastBody,
		`tools.#(name=="shell").parameters.properties.message.encrypted`).Bool(),
		"the retained top-level tool must keep its own encrypted marker")
	require.Equal(t, "sub2api_collaboration",
		gjson.GetBytes(upstream.lastBody, `input.#(type=="additional_tools").tools.0.name`).String(),
		"the migrated namespace declaration must land in input[].additional_tools")
	for index, tool := range []string{"spawn_agent", "send_message", "followup_task"} {
		base := fmt.Sprintf(`input.#(type=="additional_tools").tools.0.tools.%d`, index)
		require.Equal(t, tool, gjson.GetBytes(upstream.lastBody, base+".name").String())
		require.False(t, gjson.GetBytes(upstream.lastBody, base+".parameters.properties.message.encrypted").Exists(),
			"the migrated %s.message.encrypted must be removed after the move", tool)
	}
	require.Equal(t, []string{"tools.0.parameters.properties.message.encrypted"},
		codexV2E2EEncryptedPaths(upstream.lastBody),
		"only the three migrated collaboration markers may disappear")

	clientBody := recorder.Body.Bytes()
	codexV2E2EAssertClientParentCallMarked(t, clientBody, "output.0")
	codexV2E2EAssertClientUnmarkedCallsUntouched(t, clientBody, "output.1", "output.2")
}

func TestCodexV2ResponsesHTTPJSONDisabledGroupControl(t *testing.T) {
	codexV2E2EInstallPolicy(t)
	response := codexV2E2EJSONResponseBody()
	svc, upstream := codexV2E2ENewService(codexV2E2EUpstreamResponse("application/json", string(response)))
	c, recorder := codexV2E2ENewContext(codexV2E2EDisabledGroupID)
	account := codexV2E2EParentAccount(false)

	_, err := svc.Forward(context.Background(), c, account, codexV2E2EParentRequestBody(false))

	require.NoError(t, err)
	codexV2E2EAssertUpstreamCalledOnce(t, upstream)
	codexV2E2EAssertNonPassthroughRequest(t, c, upstream.lastReq)
	require.Equal(t, codexV2E2EEncryptedMarkerPaths, codexV2E2EEncryptedPaths(upstream.lastBody),
		"a disabled group must send the collaboration schema unchanged")
	require.Equal(t, string(response), recorder.Body.String(),
		"a disabled group must relay the upstream payload byte-for-byte")
	require.NotContains(t, recorder.Body.String(), "encrypted_function_args")
}

// ── 路径 2：SSE ──

func TestCodexV2ResponsesSSEParentPolicyE2E(t *testing.T) {
	for _, tt := range []struct {
		name       string
		singleShot bool
	}{
		{name: "complete item in output_item_added", singleShot: true},
		{name: "arguments streamed through deltas", singleShot: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			codexV2E2EInstallPolicy(t)
			stream := codexV2E2ESSEStream(tt.singleShot)
			svc, upstream := codexV2E2ENewService(codexV2E2EUpstreamResponse("text/event-stream", stream))
			c, recorder := codexV2E2ENewContext(codexV2E2EEnabledGroupID)
			account := codexV2E2EParentAccount(false)

			result, err := svc.Forward(context.Background(), c, account, codexV2E2EParentRequestBody(true))

			require.NoError(t, err)
			require.NotNil(t, result)
			codexV2E2EAssertUpstreamCalledOnce(t, upstream)
			codexV2E2EAssertNonPassthroughRequest(t, c, upstream.lastReq)
			require.Equal(t, "text/event-stream", recorder.Header().Get("Content-Type"))
			codexV2E2EAssertUpstreamSchemaRewritten(t, upstream.lastBody)
			codexV2E2EAssertStreamedClientPayload(t, recorder.Body.String(), tt.singleShot, "response.completed")
		})
	}
}

// OAuth 父端会把 collaboration namespace 摊平成 collaboration__* 再发上游。
// 本用例走同一条 SSE 处理器路径，验证上游看到扁平且已去标记的声明，
// 客户端仍拿回原 namespace 与精确 []。
func TestCodexV2ResponsesSSEOAuthFlattenNamespacesE2E(t *testing.T) {
	codexV2E2EInstallPolicy(t)
	aliasItem := func(arguments string) string {
		return strings.ReplaceAll(codexV2E2EFlatCollaborationCallItem(arguments), "collaboration__", "sub2api_collaboration__")
	}
	stream := codexV2E2ESSEStreamWithSpine(true, aliasItem, "response.completed")
	svc, upstream := codexV2E2ENewService(codexV2E2EUpstreamResponse("text/event-stream", stream))
	c, recorder := codexV2E2ENewContext(codexV2E2EEnabledGroupID)
	account := codexV2E2EParentOAuthAccount(true)

	result, err := svc.Forward(context.Background(), c, account, codexV2E2EParentRequestBody(true))

	require.NoError(t, err)
	require.NotNil(t, result)
	codexV2E2EAssertUpstreamCalledOnce(t, upstream)
	require.Equal(t, "chatgpt.com", upstream.lastReq.URL.Host)
	require.Equal(t, "/backend-api/codex/responses", upstream.lastReq.URL.Path)
	require.True(t, gjson.GetBytes(upstream.lastBody, `tools.#(name=="sub2api_collaboration__spawn_agent")`).Exists(),
		"the upstream must receive the flattened collaboration declaration")
	require.False(t, gjson.GetBytes(upstream.lastBody,
		`tools.#(name=="sub2api_collaboration__spawn_agent").parameters.properties.message.encrypted`).Exists(),
		"the flattened declaration must be rewritten before flattening")
	require.True(t, gjson.GetBytes(upstream.lastBody, `tools.#(name=="shell").parameters.properties.message.encrypted`).Bool(),
		"non-collaboration tools must stay untouched")
	require.Len(t, codexV2E2EEncryptedPaths(upstream.lastBody), 1,
		"exactly the three collaboration message markers may be removed")

	codexV2E2EAssertStreamedClientPayload(t, recorder.Body.String(), true, "response.completed")
}

// response.done 与 response.completed 在生产处理器里都是成功终态；成功终态必须同样
// 补齐完整调用项的 []，而不是被当成失败或漏补。
func TestCodexV2ResponsesSSEResponseDoneTerminalE2E(t *testing.T) {
	codexV2E2EInstallPolicy(t)
	stream := codexV2E2ESSEStreamWithSpine(true, codexV2E2ECollaborationCallItem, "response.done")
	svc, upstream := codexV2E2ENewService(codexV2E2EUpstreamResponse("text/event-stream", stream))
	c, recorder := codexV2E2ENewContext(codexV2E2EEnabledGroupID)
	account := codexV2E2EParentAccount(false)

	result, err := svc.Forward(context.Background(), c, account, codexV2E2EParentRequestBody(true))

	require.NoError(t, err, "response.done is a success terminal and must not fail the turn")
	require.NotNil(t, result)
	codexV2E2EAssertUpstreamCalledOnce(t, upstream)
	codexV2E2EAssertNonPassthroughRequest(t, c, upstream.lastReq)
	codexV2E2EAssertUpstreamSchemaRewritten(t, upstream.lastBody)
	codexV2E2EAssertStreamedClientPayload(t, recorder.Body.String(), true, "response.done")
}

func TestCodexV2ResponsesSSEDisabledGroupControl(t *testing.T) {
	codexV2E2EInstallPolicy(t)
	stream := codexV2E2ESSEStream(true)
	svc, upstream := codexV2E2ENewService(codexV2E2EUpstreamResponse("text/event-stream", stream))
	c, recorder := codexV2E2ENewContext(codexV2E2EDisabledGroupID)
	account := codexV2E2EParentAccount(false)

	_, err := svc.Forward(context.Background(), c, account, codexV2E2EParentRequestBody(true))

	require.NoError(t, err)
	codexV2E2EAssertUpstreamCalledOnce(t, upstream)
	codexV2E2EAssertNonPassthroughRequest(t, c, upstream.lastReq)
	require.Equal(t, codexV2E2EEncryptedMarkerPaths, codexV2E2EEncryptedPaths(upstream.lastBody),
		"a disabled group must send the collaboration schema unchanged")
	require.NotContains(t, recorder.Body.String(), "encrypted_function_args",
		"a disabled group must not inject the round-trip marker")
	var forwarded []string
	for _, frame := range codexV2E2EParseSSEFrames(recorder.Body.String()) {
		forwarded = append(forwarded, frame.data)
	}
	var expected []string
	for _, frame := range codexV2E2EParseSSEFrames(stream) {
		expected = append(expected, frame.data)
	}
	require.Equal(t, expected, forwarded, "a disabled group must relay every SSE payload byte-for-byte")
}

// ── 路径 3：上游 SSE、客户端非流式（SSE→JSON） ──

func TestCodexV2ResponsesSSEToJSONParentPolicyE2E(t *testing.T) {
	codexV2E2EInstallPolicy(t)
	stream := codexV2E2ESSEStream(true)
	svc, upstream := codexV2E2ENewService(codexV2E2EUpstreamResponse("text/event-stream", stream))
	c, recorder := codexV2E2ENewContext(codexV2E2EEnabledGroupID)
	account := codexV2E2EParentAccount(false)

	result, err := svc.Forward(context.Background(), c, account, codexV2E2EParentRequestBody(false))

	require.NoError(t, err)
	require.NotNil(t, result)
	codexV2E2EAssertUpstreamCalledOnce(t, upstream)
	codexV2E2EAssertNonPassthroughRequest(t, c, upstream.lastReq)
	// 与既有 handleSSEToJSON 用例一致：客户端响应头可能保留上游的
	// text/event-stream 透传值，关键判据是 body 已转换成单个 JSON。
	require.NotContains(t, recorder.Body.String(), "event:")
	codexV2E2EAssertUpstreamSchemaRewritten(t, upstream.lastBody)

	clientBody := recorder.Body.Bytes()
	require.False(t, strings.Contains(recorder.Body.String(), "data:"),
		"the upstream SSE frames must be converted to a single JSON response")
	require.Equal(t, "resp_sse_parent", gjson.GetBytes(clientBody, "id").String())
	codexV2E2EAssertClientParentCallMarked(t, clientBody, "output.0")
	codexV2E2EAssertClientUnmarkedCallsUntouched(t, clientBody, "output.1", "output.2")
	require.Equal(t, int64(19), gjson.GetBytes(clientBody, "usage.total_tokens").Int())
}

// ── 路径 4：原生 passthrough ──

// ── 失败关闭：无法确认明文时不得补标记 ──

// 父端声明改写成功后，回程仍可能拿到真密文或上游自带的加密参数集合。
// 这两类都必须保持原样：宁可少补一次 []，也不能把密文标成明文。
func TestCodexV2ResponsesParentResponseFailsClosedE2E(t *testing.T) {
	t.Run("ciphertext message is never marked", func(t *testing.T) {
		codexV2E2EInstallPolicy(t)
		ciphertext := "gAAAAABm" + strings.Repeat("Q", 440)
		item := `{"type":"function_call","id":"fc_cipher_1","call_id":"call_cipher_1","name":"spawn_agent","namespace":"collaboration","arguments":` +
			strconv.Quote(`{"message":"`+ciphertext+`"}`) + `}`
		stream := codexV2E2ECallSSEWithTerminal(item, "response.completed", "resp_failclosed")
		svc, upstream := codexV2E2ENewService(codexV2E2EUpstreamResponse("text/event-stream", stream))
		c, recorder := codexV2E2ENewContext(codexV2E2EEnabledGroupID)
		account := codexV2E2EParentAccount(false)

		_, err := svc.Forward(context.Background(), c, account, codexV2E2EParentRequestBody(true))

		require.NoError(t, err)
		codexV2E2EAssertUpstreamCalledOnce(t, upstream)
		codexV2E2EAssertUpstreamSchemaRewritten(t, upstream.lastBody)
		// 三处（added / done / completed）都必须是同一个完整调用项、都不得被补标记：
		// 只用 NotContains 单点检查会漏掉“某项被截断或丢字段”的情况。
		codexV2E2EAssertClientCallPreservedEverywhere(t, recorder.Body.String(), item, "response.completed", "")
		require.Contains(t, recorder.Body.String(), ciphertext, "the ciphertext must still reach the client")
	})

	t.Run("existing encryption argument set is preserved", func(t *testing.T) {
		codexV2E2EInstallPolicy(t)
		item := `{"type":"function_call","id":"fc_marked_1","call_id":"call_marked_1","name":"spawn_agent","namespace":"collaboration","encrypted_function_args":["opaque_1"],"arguments":` +
			strconv.Quote(codexV2E2EParentArguments) + `}`
		stream := codexV2E2ECallSSEWithTerminal(item, "response.completed", "resp_failclosed")
		svc, upstream := codexV2E2ENewService(codexV2E2EUpstreamResponse("text/event-stream", stream))
		c, recorder := codexV2E2ENewContext(codexV2E2EEnabledGroupID)
		account := codexV2E2EParentAccount(false)

		_, err := svc.Forward(context.Background(), c, account, codexV2E2EParentRequestBody(true))

		require.NoError(t, err)
		codexV2E2EAssertUpstreamCalledOnce(t, upstream)
		codexV2E2EAssertUpstreamSchemaRewritten(t, upstream.lastBody)
		codexV2E2EAssertClientCallPreservedEverywhere(t, recorder.Body.String(), item, "response.completed", `["opaque_1"]`)
	})
}

func TestCodexV2ResponsesPassthroughParentPolicyE2E(t *testing.T) {
	t.Run("streaming SSE", func(t *testing.T) {
		codexV2E2EInstallPolicy(t)
		stream := codexV2E2ESSEStream(true)
		svc, upstream := codexV2E2ENewService(codexV2E2EUpstreamResponse("text/event-stream", stream))
		c, recorder := codexV2E2ENewContext(codexV2E2EEnabledGroupID)
		account := codexV2E2EParentAccount(true)

		_, err := svc.Forward(context.Background(), c, account, codexV2E2EParentRequestBody(true))

		require.NoError(t, err)
		codexV2E2EAssertUpstreamCalledOnce(t, upstream)
		codexV2E2EAssertPassthroughRequest(t, c, upstream.lastReq)
		require.Equal(t, "text/event-stream", recorder.Header().Get("Content-Type"))
		codexV2E2EAssertUpstreamSchemaRewritten(t, upstream.lastBody)
		codexV2E2EAssertStreamedClientPayload(t, recorder.Body.String(), true, "response.completed")
	})

	t.Run("non-streaming JSON", func(t *testing.T) {
		codexV2E2EInstallPolicy(t)
		response := codexV2E2EJSONResponseBody()
		svc, upstream := codexV2E2ENewService(codexV2E2EUpstreamResponse("application/json", string(response)))
		c, recorder := codexV2E2ENewContext(codexV2E2EEnabledGroupID)
		account := codexV2E2EParentAccount(true)

		_, err := svc.Forward(context.Background(), c, account, codexV2E2EParentRequestBody(false))

		require.NoError(t, err)
		codexV2E2EAssertUpstreamCalledOnce(t, upstream)
		codexV2E2EAssertPassthroughRequest(t, c, upstream.lastReq)
		require.Equal(t, "application/json", recorder.Header().Get("Content-Type"))
		codexV2E2EAssertUpstreamSchemaRewritten(t, upstream.lastBody)
		clientBody := recorder.Body.Bytes()
		codexV2E2EAssertClientParentCallMarked(t, clientBody, "output.0")
		codexV2E2EAssertClientUnmarkedCallsUntouched(t, clientBody, "output.1", "output.2")
	})

	t.Run("upstream SSE with non-streaming client", func(t *testing.T) {
		codexV2E2EInstallPolicy(t)
		stream := codexV2E2ESSEStream(true)
		svc, upstream := codexV2E2ENewService(codexV2E2EUpstreamResponse("text/event-stream", stream))
		c, recorder := codexV2E2ENewContext(codexV2E2EEnabledGroupID)
		account := codexV2E2EParentAccount(true)

		_, err := svc.Forward(context.Background(), c, account, codexV2E2EParentRequestBody(false))

		require.NoError(t, err)
		codexV2E2EAssertUpstreamCalledOnce(t, upstream)
		codexV2E2EAssertPassthroughRequest(t, c, upstream.lastReq)
		codexV2E2EAssertUpstreamSchemaRewritten(t, upstream.lastBody)
		clientBody := recorder.Body.Bytes()
		require.Equal(t, "resp_sse_parent", gjson.GetBytes(clientBody, "id").String())
		codexV2E2EAssertClientParentCallMarked(t, clientBody, "output.0")
	})
}

// ── 失败终态：错误帧必须原样保留，不得被当成成功，也不得补 [] ──

// codexV2E2ESingleFrameStream 拼一段只含单帧的 SSE 回程。
func codexV2E2ESingleFrameStream(eventType, data string) string {
	return "event: " + eventType + "\n" + "data: " + data + "\n\n"
}

// response.failed 是失败终态。即便本次请求的父端 schema 已被改写（回程本可补
// encrypted_function_args），失败帧也必须原样保留错误字段、不得被当成成功。
func TestCodexV2ResponsesSSEResponseFailedTerminalE2E(t *testing.T) {
	codexV2E2EInstallPolicy(t)
	failed := `{"type":"response.failed","sequence_number":3,"response":{"id":"resp_failed_parent","model":"` +
		codexV2E2EModel + `","status":"failed","error":{"type":"invalid_request_error","code":"invalid_prompt","message":"the prompt is invalid"}}}`
	stream := codexV2E2ESingleFrameStream("response.failed", failed)
	svc, upstream := codexV2E2ENewService(codexV2E2EUpstreamResponse("text/event-stream", stream))
	c, recorder := codexV2E2ENewContext(codexV2E2EEnabledGroupID)
	account := codexV2E2EParentAccount(false)

	result, err := svc.Forward(context.Background(), c, account, codexV2E2EParentRequestBody(true))

	require.Error(t, err, "a failed terminal must not be reported as a successful turn")
	require.Nil(t, result)
	codexV2E2EAssertUpstreamCalledOnce(t, upstream)
	codexV2E2EAssertUpstreamSchemaRewritten(t, upstream.lastBody)

	frames := codexV2E2EParseSSEFrames(recorder.Body.String())
	failedFrames := codexV2E2EFramesOfType(frames, "response.failed")
	require.Len(t, frames, 1, "the failed stream must contain only the original failure event")
	require.Len(t, failedFrames, 1, "the failed terminal must still reach the client")
	require.JSONEq(t, failed, failedFrames[0].data, "all upstream failure fields must survive")
	require.Equal(t, "resp_failed_parent", gjson.Get(failedFrames[0].data, "response.id").String())
	require.Equal(t, "failed", gjson.Get(failedFrames[0].data, "response.status").String())
	require.Equal(t, "invalid_request_error", gjson.Get(failedFrames[0].data, "response.error.type").String(),
		"the upstream error type must survive")
	require.Equal(t, "invalid_prompt", gjson.Get(failedFrames[0].data, "response.error.code").String(),
		"the upstream error code must survive")
	require.Equal(t, "the prompt is invalid", gjson.Get(failedFrames[0].data, "response.error.message").String(),
		"the upstream error message must survive")
	require.NotContains(t, recorder.Body.String(), "encrypted_function_args",
		"a failed terminal must never gain the plaintext marker")
	require.NotContains(t, recorder.Body.String(), "response.completed",
		"a failed turn must not be rewritten into a success terminal")
}

// 裸 error 事件同样是失败：错误字段必须保留，且不得补 []、不得当成成功。
func TestCodexV2ResponsesSSEErrorTerminalE2E(t *testing.T) {
	codexV2E2EInstallPolicy(t)
	bareError := `{"type":"error","sequence_number":2,"error":{"type":"invalid_request_error","code":"invalid_prompt","message":"the prompt is invalid"}}`
	stream := codexV2E2ESingleFrameStream("error", bareError)
	svc, upstream := codexV2E2ENewService(codexV2E2EUpstreamResponse("text/event-stream", stream))
	c, recorder := codexV2E2ENewContext(codexV2E2EEnabledGroupID)
	account := codexV2E2EParentAccount(false)

	result, err := svc.Forward(context.Background(), c, account, codexV2E2EParentRequestBody(true))

	require.Error(t, err, "an error terminal must not be reported as a successful turn")
	require.Nil(t, result)
	codexV2E2EAssertUpstreamCalledOnce(t, upstream)
	codexV2E2EAssertUpstreamSchemaRewritten(t, upstream.lastBody)

	frames := codexV2E2EParseSSEFrames(recorder.Body.String())
	errorFrames := codexV2E2EFramesOfType(frames, "error")
	require.Len(t, frames, 1, "the error stream must contain only the original error event")
	require.Len(t, errorFrames, 1, "the upstream error frame must still reach the client")
	require.JSONEq(t, bareError, errorFrames[0].data, "all upstream error fields must survive")
	require.Equal(t, "invalid_request_error", gjson.Get(errorFrames[0].data, "error.type").String())
	require.Equal(t, "invalid_prompt", gjson.Get(errorFrames[0].data, "error.code").String())
	require.Equal(t, "the prompt is invalid", gjson.Get(errorFrames[0].data, "error.message").String())
	require.NotContains(t, recorder.Body.String(), "encrypted_function_args")
	require.NotContains(t, recorder.Body.String(), "response.completed")
}

// ── 本地拒绝：写入上游之前就必须失败关闭 ──

// 启用组选中非 OpenAI 子端账号、且 agent_message 是确认密文时，策略在构造任何
// 上游请求之前就返回 400：假上游必须零调用，客户端只收到固定文案（不回显密文）。
func TestCodexV2ResponsesChildFailClosedStopsBeforeUpstreamE2E(t *testing.T) {
	codexV2E2EInstallPolicy(t)
	// 上游若被误调用，recorder 不会 panic，但 requests/bodies 会非空而被断言抓到。
	svc, upstream := codexV2E2ENewService(codexV2E2EUpstreamResponse("application/json", `{}`))
	c, recorder := codexV2E2ENewContext(codexV2E2EEnabledGroupID)
	account := codexV2E2EChildAccount()
	ciphertext := "gAAAAABm" + strings.Repeat("Q", 440)
	body := []byte(`{"model":"` + codexV2E2EModel + `","stream":false,"input":[` +
		`{"type":"agent_message","content":[{"type":"encrypted_content","encrypted_content":` + strconv.Quote(ciphertext) + `}]}]}`)

	result, err := svc.Forward(context.Background(), c, account, body)

	require.Error(t, err)
	require.Nil(t, result)
	require.True(t, IsCodexV2GroupPolicyRejectionError(err), "this must be a local policy rejection, got %v", err)
	codexV2E2EAssertUpstreamNotCalled(t, upstream)
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Equal(t, codexV2ChildPolicyClientMessage,
		gjson.GetBytes(recorder.Body.Bytes(), "error.message").String(),
		"the client must get the fixed message, never the ciphertext")
	require.NotContains(t, recorder.Body.String(), ciphertext, "the rejected ciphertext must not be echoed")
	require.JSONEq(t,
		`{"error":{"type":"invalid_request_error","message":`+strconv.Quote(codexV2ChildPolicyClientMessage)+`,"param":"input"}}`,
		recorder.Body.String(),
		"the local rejection must be the fixed 400 payload and nothing else")
}
