package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Codex MultiAgentV2 一期跨模型兼容的协议常量。
//
// 裁决依据（orchestration/2026-09-29-codex-v2-compat/decision-2-output.txt）：
// 只针对官方客户端默认的 collaboration 命名空间与三个带 message 参数的协作工具，
// 不扩展到自定义 namespace（未经实测不应声称支持）。
const (
	codexV2CollaborationNamespace = "collaboration"

	// codexV2ParentPlaintextAttemptContextKey 标记“本次上游尝试确实改写了
	// collaboration schema”，回程只有在这条尝试上才允许补 encrypted_function_args。
	codexV2ParentPlaintextAttemptContextKey = "codex_v2_parent_plaintext_attempt"

	// codexV2PolicyDecisionContextKey 固定单个 HTTP 请求或单条 WS 连接的策略快照。
	codexV2PolicyDecisionContextKey = "codex_v2_group_policy_decision"

	// codexV2WSv2UnsupportedMessage 是启用分组选中原生 WSv2 上游时的固定拒绝文案。
	// 一期只支持 HTTP JSON/SSE 与已走 HTTP bridge 的 WS，不声称 WSv2 兼容。
	codexV2WSv2UnsupportedMessage = "codex v2 group policy: native Responses WebSocket v2 is unsupported for this group in phase 1"

	// codexV2ChildPolicyClientMessage 是子端密文 fail-closed 时回传给客户端的文案。
	codexV2ChildPolicyClientMessage = "codex v2 group policy: the agent message is encrypted or cannot be confirmed as plaintext, and the selected upstream cannot decode it"

	// CodexV2ForcedChatCompletionsClientMessage 是启用组选中 forced Chat Completions
	// 回退时的固定客户端文案。该回退在请求被改写成 Responses→Chat 前就把工具声明
	// 与消息转成 Chat 形态，回程没有 encrypted_function_args 的注入点，一期不支持。
	CodexV2ForcedChatCompletionsClientMessage = "codex v2 group policy: the selected OpenAI account is forced to Chat Completions, which this group does not support in phase 1; use a native Responses upstream"

	// codexV2UnsafeToolInheritanceMessage 是连接级工具继承无法安全重建时的固定文案。
	codexV2UnsafeToolInheritanceMessage = "codex v2 group policy: this connection's inherited tool declaration cannot be safely rebuilt for the selected account; start a new connection"
)

// codexV2CollaborationMessageTools 是 V2 协作工具中带 message 参数的三项工具。
// 其余协作工具（wait_agent、list_agents 等）没有 message，保持原样。
var codexV2CollaborationMessageTools = map[string]struct{}{
	"spawn_agent":   {},
	"send_message":  {},
	"followup_task": {},
}

// codexV2ChildMessagePolicyError 表示子端 agent_message 无法安全改写。
// 错误文本只说明原因分类，绝不包含消息正文或密文。
type codexV2ChildMessagePolicyError struct {
	reason string
}

// errCodexV2WSv2Unsupported 标记“一期策略拒绝 WSv2 上游”这一类本地拒绝，
// 供上层区分账号/上游异常：本地策略拒绝不参与账号健康与调度失败统计。
var errCodexV2WSv2Unsupported = errors.New("codex v2 group policy: native Responses WebSocket v2 is unsupported")

// ErrCodexV2ForcedChatCompletionsUnsupported 标记“一期策略拒绝 forced Chat
// Completions 回退”这一类本地拒绝，同样不参与账号健康与调度失败统计。
var ErrCodexV2ForcedChatCompletionsUnsupported = errors.New("codex v2 group policy: forced Chat Completions fallback is unsupported")

// errCodexV2UnsafeToolInheritance 标记“连接里有继承的上游工具声明，但缺少可用的
// 客户端原始声明来源，无法按本次账号安全重建”的本地拒绝。它同样不计账号失败。
var errCodexV2UnsafeToolInheritance = errors.New("codex v2 group policy: inherited tool declaration cannot be safely rebuilt")

func (e *codexV2ChildMessagePolicyError) Error() string {
	if e == nil {
		return "codex v2 child agent message rejected"
	}
	return "codex v2 child agent message rejected: " + e.reason
}

func newCodexV2ChildMessagePolicyError(reason string) error {
	return &codexV2ChildMessagePolicyError{reason: reason}
}

// codexV2PolicyDecision 是单个请求/连接固定的策略判定。
type codexV2PolicyDecision struct {
	groupID int64
	enabled bool
}

// codexV2RequestAPIKeyGroupID 读取已鉴权 API Key 的不可变分组 ID。
func codexV2RequestAPIKeyGroupID(c *gin.Context) (int64, bool) {
	if c == nil {
		return 0, false
	}
	value, exists := c.Get("api_key")
	if !exists {
		return 0, false
	}
	apiKey, ok := value.(*APIKey)
	if !ok || apiKey == nil || apiKey.GroupID == nil {
		return 0, false
	}
	return *apiKey.GroupID, true
}

// codexV2PolicyDecisionForRequest 返回本 HTTP 请求或本 WS 连接固定的策略判定。
// 首次调用即固定结果，后续 failover 重试与 WS 多轮复用同一份快照，重载只对新
// 请求/新连接生效。
func codexV2PolicyDecisionForRequest(c *gin.Context) codexV2PolicyDecision {
	if c == nil {
		return codexV2PolicyDecision{}
	}
	if value, exists := c.Get(codexV2PolicyDecisionContextKey); exists {
		if decision, ok := value.(codexV2PolicyDecision); ok {
			return decision
		}
	}
	groupID, hasGroup := codexV2RequestAPIKeyGroupID(c)
	decision := codexV2PolicyDecision{groupID: groupID}
	if hasGroup {
		decision.enabled = CurrentCodexV2GroupPolicy().EnabledForGroupID(groupID)
	}
	c.Set(codexV2PolicyDecisionContextKey, decision)
	return decision
}

// codexV2PolicyEnabledForRequest 报告本请求/连接是否启用一期策略。
func codexV2PolicyEnabledForRequest(c *gin.Context) bool {
	return codexV2PolicyDecisionForRequest(c).enabled
}

func clearCodexV2ParentPlaintextAttempt(c *gin.Context) {
	if c == nil {
		return
	}
	c.Set(codexV2ParentPlaintextAttemptContextKey, false)
}

func markCodexV2ParentPlaintextAttempt(c *gin.Context) {
	if c == nil {
		return
	}
	c.Set(codexV2ParentPlaintextAttemptContextKey, true)
}

func codexV2ParentPlaintextAttemptActive(c *gin.Context) bool {
	if c == nil {
		return false
	}
	value, exists := c.Get(codexV2ParentPlaintextAttemptContextKey)
	if !exists {
		return false
	}
	active, _ := value.(bool)
	return active
}

// codexV2PolicyOutcome 是一次出站请求的策略结果。
type codexV2PolicyOutcome struct {
	Body []byte
	// ParentPlaintextSchema 报告本次真正发往父端上游的工具声明是“已移除
	// collaboration message.encrypted 标记”的形态，回程据此补 encrypted_function_args。
	ParentPlaintextSchema bool
}

// applyCodexV2UpstreamRequestPolicies 在向上游写入前应用分组策略。
//
//   - OpenAI 上游（父端）：只移除 collaboration 三个工具 message 参数的 encrypted
//     标记，并记录本次尝试，供回程补 encrypted_function_args: []。
//   - 非 OpenAI 上游（子端）：把完整、非空的明文 agent_message 换成普通 user
//     消息；真密文或不确定内容直接报错（fail-closed）。
//
// 未启用分组、无 API Key 分组、非目标请求一律原样返回。
func applyCodexV2UpstreamRequestPolicies(c *gin.Context, account *Account, body []byte) ([]byte, error) {
	outcome, err := applyCodexV2UpstreamRequestPoliciesForAttempt(c, account, body)
	if err != nil {
		return body, err
	}
	return outcome.Body, nil
}

// applyCodexV2UpstreamRequestPoliciesForAttempt 与 applyCodexV2UpstreamRequestPolicies
// 语义一致，额外回报父端“本次上游声明已去 encrypted 标记”的判定结果。
//
// 该结果只由“本帧实际出站的声明 + 本次实际选中的账号”决定，不继承上一轮：
// 调用方若要让续帧按账号重新推导，必须先把客户端原始声明重新注入本帧。
func applyCodexV2UpstreamRequestPoliciesForAttempt(
	c *gin.Context,
	account *Account,
	body []byte,
) (codexV2PolicyOutcome, error) {
	clearCodexV2ParentPlaintextAttempt(c)
	if len(body) == 0 || account == nil || !codexV2PolicyEnabledForRequest(c) {
		return codexV2PolicyOutcome{Body: body}, nil
	}
	rewritten, _, err := rewriteCodexV2ChildAgentMessages(body)
	if err != nil {
		return codexV2PolicyOutcome{Body: body}, err
	}
	rewritten, applied, err := rewriteCodexV2ParentPlaintextToolSchema(rewritten)
	if err != nil {
		return codexV2PolicyOutcome{Body: body}, err
	}
	if applied {
		markCodexV2ParentPlaintextAttempt(c)
	}
	return codexV2PolicyOutcome{Body: rewritten, ParentPlaintextSchema: applied}, nil
}

// applyCodexV2ParentResponsePolicy 在回程补精确的 encrypted_function_args: []。
// 只在本次尝试改写过 collaboration schema 时生效；无法确认明文抄写时保持原样。
func applyCodexV2ParentResponsePolicy(c *gin.Context, payload []byte) []byte {
	if !codexV2ParentPlaintextAttemptActive(c) {
		return payload
	}
	return insertCodexV2PlaintextFunctionCallMarkers(restoreCodexV2NamespaceCalls(payload))
}

// codexV2ClientPolicyErrorMessage 把策略错误映射为不包含正文的固定客户端文案。
func codexV2ClientPolicyErrorMessage(err error) string {
	var childErr *codexV2ChildMessagePolicyError
	if errors.As(err, &childErr) {
		return codexV2ChildPolicyClientMessage
	}
	if errors.Is(err, ErrCodexV2ForcedChatCompletionsUnsupported) {
		return CodexV2ForcedChatCompletionsClientMessage
	}
	if errors.Is(err, errCodexV2UnsafeToolInheritance) {
		return codexV2UnsafeToolInheritanceMessage
	}
	return "codex v2 group policy rejected this request"
}

// newCodexV2UnsafeToolInheritanceError 用于 WS→HTTP bridge：连接里有继承的上游工具
// 声明但缺少客户端原始声明来源，无法按本次账号安全重建，必须在写入上游前拒绝。
func newCodexV2UnsafeToolInheritanceError() error {
	return fmt.Errorf("%w: %s", errCodexV2UnsafeToolInheritance, codexV2UnsafeToolInheritanceMessage)
}

// codexV2ClientToolsDeclarationSource 返回可作为连接级来源保存的客户端工具声明。
// 只接受 JSON 数组：其他形态（缺失、null、对象等）不构成可重建的声明来源。
func codexV2ClientToolsDeclarationSource(raw []byte) json.RawMessage {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '[' || !json.Valid(trimmed) {
		return nil
	}
	return append(json.RawMessage(nil), trimmed...)
}

// codexV2ToolsDeclarationHasEntries 报告一份工具声明是否真的列出了工具。
// 空数组（真实 Codex 在只使用 Lite 载体时会带顶层 "tools": []）不算声明。
func codexV2ToolsDeclarationHasEntries(raw json.RawMessage) bool {
	if len(bytes.TrimSpace(raw)) == 0 {
		return false
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return false
	}
	return len(items) > 0
}

// codexV2ClientAdditionalToolsSource 提取 input[] 中 additional_tools 项的原始 JSON。
//
// 实际安装版 Codex 0.158 把 collaboration 声明放在这里（顶层 tools 为空）。连接需要
// 保存这份客户端原始声明，续帧省略声明时才能按本次账号重建，而不是沿用上一账号的
// 派生形态或让声明静默消失。没有该项时返回 nil。
func codexV2ClientAdditionalToolsSource(body []byte) []json.RawMessage {
	if len(body) == 0 ||
		!bytes.Contains(body, []byte(`"additional_tools"`)) ||
		!json.Valid(body) {
		return nil
	}
	input := parseRawJSONView(body).Get("input")
	if !input.IsArray() {
		return nil
	}
	var items []json.RawMessage
	input.ForEach(func(_, item gjson.Result) bool {
		if !item.IsObject() || strings.TrimSpace(item.Get("type").String()) != "additional_tools" {
			return true
		}
		items = append(items, append(json.RawMessage(nil), item.Raw...))
		return true
	})
	return items
}

// codexV2MarshalRawItems 把若干个原始 JSON 对象拼成一个 JSON 数组；空输入返回 nil。
func codexV2MarshalRawItems(items []json.RawMessage) json.RawMessage {
	if len(items) == 0 {
		return nil
	}
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i, item := range items {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(item)
	}
	buf.WriteByte(']')
	if !json.Valid(buf.Bytes()) {
		return nil
	}
	return json.RawMessage(buf.Bytes())
}

// codexV2ReinjectAdditionalTools 把连接保存的客户端 Lite 声明按原顺序放回 input[] 头部，
// 并去掉本帧已有的 additional_tools 项（续帧省略场景下通常没有；显式替换场景不会走到
// 这里）。input 不是数组时无法保持原有载体，返回错误交由调用方 fail-closed。
func codexV2ReinjectAdditionalTools(body []byte, items []json.RawMessage) ([]byte, error) {
	if len(items) == 0 {
		return body, nil
	}
	input := parseRawJSONView(body).Get("input")
	if !input.IsArray() {
		return body, errors.New("codex v2 group policy: request input is not an array, cannot restore the inherited tool declaration")
	}
	var rebuilt bytes.Buffer
	rebuilt.WriteByte('[')
	first := true
	writeRaw := func(raw string) {
		if !first {
			rebuilt.WriteByte(',')
		}
		first = false
		rebuilt.WriteString(raw)
	}
	for _, item := range items {
		writeRaw(string(item))
	}
	input.ForEach(func(_, item gjson.Result) bool {
		if item.IsObject() && strings.TrimSpace(item.Get("type").String()) == "additional_tools" {
			return true
		}
		writeRaw(item.Raw)
		return true
	})
	rebuilt.WriteByte(']')
	updated, err := sjson.SetRawBytes(body, "input", rebuilt.Bytes())
	if err != nil {
		return body, fmt.Errorf("restore codex v2 inherited tool declaration: %w", err)
	}
	return updated, nil
}

// WriteCodexV2RequestPolicyError 在向上游写入前返回明确的 400，不暴露消息正文。
//
// 这是本地请求拒绝，不是上游错误：只标 client business limited 让 ops 归到
// client 侧，不写上游错误上下文；MarkOpsLocalClientRejection 还会让同一请求里先前
// 尝试留下的上游错误上下文不再决定最终归因（诊断事件仍保留）；同时用
// MarkResponseCommitted 阻止 handler 在 JSON 之后追加 response.failed 流内错误帧。
func WriteCodexV2RequestPolicyError(c *gin.Context, err error) {
	if c == nil {
		return
	}
	message := codexV2ClientPolicyErrorMessage(err)
	MarkOpsClientBusinessLimited(c, OpsClientBusinessLimitedReasonLocalFeatureGate)
	MarkOpsLocalClientRejection(c)
	MarkResponseCommitted(c)
	c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{
		"type":    "invalid_request_error",
		"message": message,
		"param":   "input",
	}})
}

// writeCodexV2WSv2UnsupportedError 在向上游写入前拒绝 HTTP 入站转 WSv2。
func writeCodexV2WSv2UnsupportedError(c *gin.Context) {
	if c == nil {
		return
	}
	MarkOpsClientBusinessLimited(c, OpsClientBusinessLimitedReasonLocalFeatureGate)
	MarkOpsLocalClientRejection(c)
	MarkResponseCommitted(c)
	c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{
		"type":    "invalid_request_error",
		"message": codexV2WSv2UnsupportedMessage,
	}})
}

// newCodexV2ForcedChatCompletionsRejectionError 用于 F3：启用组选中的 OpenAI
// 平台账号实际路由为 forced Chat Completions 回退时，本地拒绝而不是半套改写。
func newCodexV2ForcedChatCompletionsRejectionError() error {
	return fmt.Errorf("%w: %s", ErrCodexV2ForcedChatCompletionsUnsupported, CodexV2ForcedChatCompletionsClientMessage)
}

// codexV2ForcedChatCompletionsRejection 报告本次尝试是否必须因 forced
// Chat Completions 回退被本地拒绝。
//
// 判据按“实际选中的上游协议”而不是 platform：只有 OpenAI 平台账号走
// Responses→Chat Completions 回退时，父端 schema 改写会缺少 encrypted_function_args
// 的注入点。CN/OpenCode 等非 OpenAI 平台走 Chat bridge 属于子端支持范围。
func codexV2ForcedChatCompletionsRejection(c *gin.Context, account *Account) error {
	if account == nil || !account.IsOpenAI() {
		return nil
	}
	if !shouldForwardOpenAIResponsesViaRawChatCompletions(account) {
		return nil
	}
	if !codexV2PolicyEnabledForRequest(c) {
		return nil
	}
	return newCodexV2ForcedChatCompletionsRejectionError()
}

// newCodexV2WSv2UnsupportedClientError 用于 WS 入站路径：返回明确的策略拒绝帧。
func newCodexV2WSv2UnsupportedClientError() error {
	closeErr := NewOpenAIWSClientCloseError(coderws.StatusPolicyViolation, codexV2WSv2UnsupportedMessage, nil)
	return fmt.Errorf("%w: %w", errCodexV2WSv2Unsupported, closeErr)
}

// IsCodexV2GroupPolicyRejectionError 报告错误是否由一期分组策略在本地拒绝产生。
// 本地拒绝与账号凭据和上游健康无关，调用方不应据此惩罚账号。
func IsCodexV2GroupPolicyRejectionError(err error) bool {
	if err == nil {
		return false
	}
	var childErr *codexV2ChildMessagePolicyError
	if errors.As(err, &childErr) {
		return true
	}
	return errors.Is(err, errCodexV2WSv2Unsupported) ||
		errors.Is(err, ErrCodexV2ForcedChatCompletionsUnsupported) ||
		errors.Is(err, errCodexV2NamespaceCollision) ||
		errors.Is(err, errCodexV2UnsafeToolInheritance)
}

// codexV2WSv2GuardError 是 WS 入站的一致守卫。httpBridgeOnly 报告这条连接是否
// 已被确认全程只走 HTTP bridge、永不向上游 WS 拨号或写帧。
//
// 只有 httpBridgeOnly 为 true 才放行启用分组；其余形态（passthrough、
// ctx_pool/shared/dedicated、以及会在 warmup/后续轮切入 WS 的 adaptive）
// 都在首帧写入上游前返回明确的策略拒绝。
func codexV2WSv2GuardError(c *gin.Context, httpBridgeOnly bool) error {
	return nil
}

// codexV2WSv2HTTPBridgeOnly 判定一条 WS ingress 连接是否保证全程不向上游 WS
// 拨号或写帧。useHTTPBridge 是 ingress 的现有判定；adaptive 即使命中它，也会在
// warmup 或后续放得下的轮次重新切到原生 WS，因此只有 adaptive 的上游本身没有
// WSv2 能力（wsAvailable=false）时才算纯 HTTP。
func codexV2WSv2HTTPBridgeOnly(
	useHTTPBridge bool,
	ingressMode string,
	upstreamTransport OpenAIUpstreamTransport,
) bool {
	if !useHTTPBridge {
		return false
	}
	if ingressMode == OpenAIWSIngressModeAdaptive &&
		upstreamTransport == OpenAIUpstreamTransportResponsesWebsocketV2 {
		return false
	}
	return true
}

// rewriteCodexV2ParentPlaintextToolSchema 移除 collaboration 命名空间下
// spawn_agent / send_message / followup_task 三个工具 message 参数的 encrypted
// 标记（属性级）。其他工具、其他命名空间与其余字段保持不变。
//
// 返回值：改写后的 body、是否发生改写、错误。
func rewriteCodexV2ParentPlaintextToolSchema(body []byte) ([]byte, bool, error) {
	if len(body) == 0 ||
		!bytes.Contains(body, []byte(codexV2CollaborationNamespace)) ||
		!json.Valid(body) {
		return body, false, nil
	}
	paths := codexV2EncryptedMessageMarkerPaths(parseRawJSONView(body))
	rewritten := body
	for _, path := range paths {
		updated, err := sjson.DeleteBytes(rewritten, path)
		if err != nil {
			return body, false, fmt.Errorf("strip codex v2 encrypted marker: %w", err)
		}
		rewritten = updated
	}
	return rewriteCodexV2NamespaceDeclarationsAndHistory(rewritten)
}

// codexV2EncryptedMessageMarkerPaths 定位需要删除的属性级 encrypted 标记。
// 支持顶层 tools[] 与 Responses Lite 的 input[].additional_tools[].tools[] 两种载体。
func codexV2EncryptedMessageMarkerPaths(root gjson.Result) []string {
	paths := make([]string, 0, len(codexV2CollaborationMessageTools))
	collect := func(base string, tools gjson.Result) {
		if !tools.IsArray() {
			return
		}
		namespaceIndex := 0
		tools.ForEach(func(_, tool gjson.Result) bool {
			current := namespaceIndex
			namespaceIndex++
			if strings.TrimSpace(tool.Get("type").String()) != "namespace" {
				return true
			}
			if strings.TrimSpace(tool.Get("name").String()) != codexV2CollaborationNamespace {
				return true
			}
			children := tool.Get("tools")
			if !children.IsArray() {
				return true
			}
			childIndex := 0
			children.ForEach(func(_, child gjson.Result) bool {
				childCurrent := childIndex
				childIndex++
				if strings.TrimSpace(child.Get("type").String()) != "function" {
					return true
				}
				if !codexV2IsCollaborationMessageTool(strings.TrimSpace(child.Get("name").String())) {
					return true
				}
				if !child.Get("parameters.properties.message.encrypted").Exists() {
					return true
				}
				paths = append(paths, fmt.Sprintf(
					"%s.%d.tools.%d.parameters.properties.message.encrypted",
					base, current, childCurrent,
				))
				return true
			})
			return true
		})
	}
	collect("tools", root.Get("tools"))
	input := root.Get("input")
	if input.IsArray() {
		itemIndex := 0
		input.ForEach(func(_, item gjson.Result) bool {
			current := itemIndex
			itemIndex++
			if strings.TrimSpace(item.Get("type").String()) != "additional_tools" {
				return true
			}
			collect(fmt.Sprintf("input.%d.tools", current), item.Get("tools"))
			return true
		})
	}
	return paths
}

func codexV2IsCollaborationMessageTool(name string) bool {
	_, ok := codexV2CollaborationMessageTools[name]
	return ok
}

// insertCodexV2PlaintextFunctionCallMarkers 给完整 function_call 项补
// encrypted_function_args: []：仅当工具属于 collaboration 三项、参数是完整 JSON，
// 且 arguments.message 已确认是明文。Delta 事件与密文参数保持原样。
//
// 以 gjson 定位、sjson 定点写入，不整体反序列化重建，未知字段与数字格式不会丢失。
func insertCodexV2PlaintextFunctionCallMarkers(payload []byte) []byte {
	if len(payload) == 0 ||
		!bytes.Contains(payload, []byte("function_call")) ||
		!bytes.Contains(payload, []byte(`"arguments"`)) ||
		!json.Valid(payload) {
		return payload
	}
	paths := collectCodexV2PlaintextFunctionCallPaths(parseRawJSONView(payload))
	if len(paths) == 0 {
		return payload
	}
	rewritten := payload
	for _, path := range paths {
		updated, err := sjson.SetRawBytes(rewritten, path+".encrypted_function_args", []byte("[]"))
		if err != nil {
			return payload
		}
		rewritten = updated
	}
	return rewritten
}

// collectCodexV2PlaintextFunctionCallPaths 在 Responses 回程载荷里定位完整调用项。
// 只沿已知容器字段（output/item/response/data）下探，不递归任意字段。
func collectCodexV2PlaintextFunctionCallPaths(root gjson.Result) []string {
	return collectCodexV2FunctionCallPaths(root, codexV2IsPlaintextCollaborationCall)
}

func collectCodexV2FunctionCallPaths(root gjson.Result, matches func(gjson.Result) bool) []string {
	paths := make([]string, 0, 2)
	var walk func(node gjson.Result, prefix string, depth int)
	walk = func(node gjson.Result, prefix string, depth int) {
		if depth > 4 {
			return
		}
		switch {
		case node.IsArray():
			index := 0
			node.ForEach(func(_, child gjson.Result) bool {
				childPrefix := codexV2JoinJSONPath(prefix, strconv.Itoa(index))
				index++
				walk(child, childPrefix, depth+1)
				return true
			})
		case node.IsObject():
			if matches(node) {
				paths = append(paths, prefix)
			}
			for _, key := range []string{"output", "item", "response", "data"} {
				child := node.Get(key)
				if !child.Exists() {
					continue
				}
				walk(child, codexV2JoinJSONPath(prefix, key), depth+1)
			}
		}
	}
	walk(root, "", 0)
	return paths
}

// codexV2JoinJSONPath 拼接 gjson/sjson 路径。根节点是数组时要得到 "0.…"，
// 不能生成 ".0.…" 这种带前导点、sjson 无法解析的路径。
func codexV2JoinJSONPath(prefix, segment string) string {
	if prefix == "" {
		return segment
	}
	return prefix + "." + segment
}

func codexV2IsPlaintextCollaborationCall(item gjson.Result) bool {
	if strings.TrimSpace(item.Get("type").String()) != "function_call" {
		return false
	}
	name := strings.TrimSpace(item.Get("name").String())
	if !codexV2IsCollaborationCallName(name) {
		return false
	}
	// 上游若已显式声明本调用的加密参数集合，一律不覆盖：只有非空列表以外的
	// 形态才允许补精确的 []。
	if existing := item.Get("encrypted_function_args"); existing.Exists() && existing.Raw != "[]" {
		return false
	}
	// 身份判定必须能证明这是 collaboration 调用：要么显式带
	// namespace=collaboration，要么是摊平后仍保留命名空间前缀的名字。裸名
	// spawn_agent/send_message/followup_task 可能属于同名的顶层非协作工具，
	// 不能靠名字猜。
	namespace := strings.TrimSpace(item.Get("namespace").String())
	switch {
	case namespace == codexV2CollaborationNamespace:
		if !codexV2IsCollaborationMessageTool(name) {
			return false
		}
	case namespace == "" && strings.HasPrefix(name, codexV2CollaborationNamespace+"__"):
	case namespace != "":
		return false
	default:
		return false
	}
	arguments := item.Get("arguments")
	if arguments.Type != gjson.String {
		return false
	}
	message := gjson.Get(arguments.String(), "message")
	if message.Type != gjson.String {
		return false
	}
	return codexV2MessageLooksPlaintext(message.String())
}

// codexV2IsCollaborationCallName 接受还原后的裸名与摊平名两种形态。
func codexV2IsCollaborationCallName(name string) bool {
	if codexV2IsCollaborationMessageTool(name) {
		return true
	}
	flatPrefix := codexV2CollaborationNamespace + "__"
	if !strings.HasPrefix(name, flatPrefix) {
		return false
	}
	rest := strings.TrimPrefix(name, flatPrefix)
	if codexV2IsCollaborationMessageTool(rest) {
		return true
	}
	// 超长摊平名带 8 字节哈希后缀（见 apicompat.flattenNamespaceToolName）。
	for tool := range codexV2CollaborationMessageTools {
		if strings.HasPrefix(rest, tool+"__") {
			return true
		}
	}
	return false
}

// codexV2MessageLooksPlaintext 判断 message 参数是否为可确认的明文。
// 失败关闭：空值、Fernet 前缀或长 base64 token 都视为不可确认，不补明文标记。
func codexV2MessageLooksPlaintext(value string) bool {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return false
	}
	if strings.HasPrefix(trimmed, "gAAAA") {
		return false
	}
	return !codexV2LooksLikeBase64Token(trimmed)
}

// codexV2LooksLikeBase64Token 报告字符串是否形如无空白的长 base64 载荷。
// 长度阈值取 64 字节：GPT 父端密文实测 440 字节，正常任务文本含空白或非
// base64 字符（例如中文）。
func codexV2LooksLikeBase64Token(value string) bool {
	if len(value) < 64 || strings.ContainsAny(value, " \t\r\n") {
		return false
	}
	for _, ch := range value {
		switch {
		case ch >= 'A' && ch <= 'Z':
		case ch >= 'a' && ch <= 'z':
		case ch >= '0' && ch <= '9':
		case ch == '+' || ch == '/' || ch == '=' || ch == '-' || ch == '_':
		default:
			return false
		}
	}
	return true
}
