package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	openAIWSClientReadLimitBytesDefault     int64 = 64 * 1024 * 1024
	openAIWSHTTPBridgeThresholdBytesDefault int64 = 15 * 1024 * 1024
	openAIWSHTTPBridgeErrorBodyLimitBytes         = 64 * 1024
)

const openAIWSHTTPBridgeToolStateContextKey = "openai_ws_http_bridge_tool_state"

type openAIWSHTTPBridgeToolState struct {
	ClientMapping apicompat.ResponsesClientToolMapping
	// ClientTools 保存连接级“客户端原始形态”的顶层 tools 声明（保留
	// collaboration message.encrypted）。
	ClientTools json.RawMessage
	// ClientAdditionalTools 保存客户端在 input[].additional_tools[] 里的原始声明；
	// 实际安装版 Codex 0.158 的 collaboration 就挂在这里。两个载体一起构成“客户端
	// 当前声明”，续帧省略声明时按本次实际选中的账号重新推导，避免上一账号派生出的
	// 形态（已去标记或已摊平）泄漏到另一个账号，也避免声明静默消失。
	ClientAdditionalTools json.RawMessage
	// LoweredTools 是顶层声明按当前账号派生的出站形态（已摊平/降级）。
	LoweredTools json.RawMessage
	// LoweredAdditionalTools 是 Lite 载体按当前账号派生的出站形态。
	LoweredAdditionalTools json.RawMessage
	// CodexV2ParentPlaintextSchema 是 LoweredTools 的同源派生元数据：该声明在
	// 出站给父端时移除了 collaboration message.encrypted 标记。它只作诊断与回归
	// 断言使用；每次尝试都会按实际账号重新推导并覆盖，绝不跨账号沿用。
	CodexV2ParentPlaintextSchema bool
}

func openAIWSHTTPBridgeToolStateFromContext(c *gin.Context) (openAIWSHTTPBridgeToolState, bool) {
	if c == nil {
		return openAIWSHTTPBridgeToolState{}, false
	}
	value, ok := c.Get(openAIWSHTTPBridgeToolStateContextKey)
	state, typed := value.(openAIWSHTTPBridgeToolState)
	return state, ok && typed
}

func setOpenAIWSHTTPBridgeToolState(c *gin.Context, state openAIWSHTTPBridgeToolState) {
	if c == nil {
		return
	}
	state.ClientTools = append(json.RawMessage(nil), state.ClientTools...)
	state.ClientAdditionalTools = append(json.RawMessage(nil), state.ClientAdditionalTools...)
	state.LoweredTools = append(json.RawMessage(nil), state.LoweredTools...)
	state.LoweredAdditionalTools = append(json.RawMessage(nil), state.LoweredAdditionalTools...)
	c.Set(openAIWSHTTPBridgeToolStateContextKey, state)
}

func decodeOpenAIWSHTTPBridgeLoweredTools(raw json.RawMessage) []any {
	if len(raw) == 0 {
		return nil
	}
	var tools []any
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil
	}
	return tools
}

// decodeOpenAIWSHTTPBridgeRawItems 解析连接里保存的原始 JSON 项数组。
func decodeOpenAIWSHTTPBridgeRawItems(raw json.RawMessage) []json.RawMessage {
	if len(json.RawMessage(bytes.TrimSpace(raw))) == 0 {
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil
	}
	return items
}

func openAIWSHTTPBridgeRawField(body []byte, name string) (json.RawMessage, bool) {
	// Validate the whole document, but borrow immutable views during the scan.
	// Copy only the selected field: saved tools must not retain the large input.
	if !json.Valid(body) {
		return nil, false
	}
	root := parseRawJSONView(body)
	if !root.IsObject() {
		return nil, false
	}
	var selected gjson.Result
	present := false
	root.ForEach(func(key, value gjson.Result) bool {
		// Match encoding/json's last duplicate key and escaped-key semantics.
		if key.String() == name {
			selected, present = value, true
		}
		return true
	})
	if !present {
		return nil, false
	}
	return json.RawMessage(selected.Raw), true
}

func openAIWSHTTPBridgeToolUpstreamName(account *Account) string {
	if account != nil && account.Platform == PlatformGrok {
		return "Grok WS HTTP bridge"
	}
	return "OpenAI WS HTTP bridge"
}

// writeCodexV2WSBridgePolicyError 把 Codex V2 的本地策略拒绝以固定文案错误帧
// 写给 WS 客户端。文案不包含请求正文；失败时由调用方按原错误返回。
func writeCodexV2WSBridgePolicyError(c *gin.Context, writeClientMessage func([]byte) error, err error) {
	if writeClientMessage == nil {
		return
	}
	MarkOpsClientBusinessLimited(c, OpsClientBusinessLimitedReasonLocalFeatureGate)
	MarkOpsLocalClientRejection(c)
	message := codexV2ClientPolicyErrorMessage(err)
	clientError := buildOpenAIWSHTTPBridgeErrorEvent(http.StatusBadRequest, message)
	if writeErr := writeClientMessage(clientError); writeErr == nil {
		markOpenAIWSClientVisibleFailure(c, "error", clientError)
	}
}

// ResolveOpenAIWSClientFirstMessageTimeout returns the first-message inactivity window.
func ResolveOpenAIWSClientFirstMessageTimeout(cfg *config.Config) time.Duration {
	seconds := config.DefaultOpenAIWSClientFirstMessageTimeoutSeconds
	if cfg != nil && cfg.Gateway.OpenAIWS.ClientFirstMessageTimeoutSeconds > 0 {
		seconds = cfg.Gateway.OpenAIWS.ClientFirstMessageTimeoutSeconds
	}
	return time.Duration(seconds) * time.Second
}

func ResolveOpenAIWSClientReadLimitBytes(cfg *config.Config) int64 {
	if cfg == nil || cfg.Gateway.OpenAIWS.ClientReadLimitBytes <= 0 {
		return openAIWSClientReadLimitBytesDefault
	}
	return cfg.Gateway.OpenAIWS.ClientReadLimitBytes
}

func (s *OpenAIGatewayService) openAIWSHTTPBridgeEnabled() bool {
	return s != nil && s.cfg != nil && s.cfg.Gateway.OpenAIWS.HTTPBridgeEnabled
}

func (s *OpenAIGatewayService) openAIWSHTTPBridgeThresholdBytes() int64 {
	if s == nil || s.cfg == nil || s.cfg.Gateway.OpenAIWS.HTTPBridgeThresholdBytes <= 0 {
		return openAIWSHTTPBridgeThresholdBytesDefault
	}
	return s.cfg.Gateway.OpenAIWS.HTTPBridgeThresholdBytes
}

// Keep this list explicit: a third-party model with a GPT-looking name must
// not silently inherit the route threshold without a verified profile.
var openAIWSGPTThresholdModels = map[string]struct{}{
	"gpt-4o": {}, "gpt-4o-mini": {}, "gpt-4.1": {}, "gpt-4.1-mini": {},
	"gpt-4.1-nano": {}, "gpt-4.5": {}, "gpt-5": {}, "gpt-5.1": {},
	"gpt-5.2": {}, "gpt-5.3": {}, "gpt-5.3-codex": {}, "gpt-5.3-codex-spark": {},
	"gpt-5.4": {}, "gpt-5.4-mini": {}, "gpt-5.4-nano": {}, "gpt-5.5": {},
	"gpt-5.5-pro": {}, "gpt-5.6-sol": {}, "gpt-5.6-terra": {}, "gpt-5.6-luna": {},
	"gpt-6-astra": {}, "gpt-6-sol": {}, "gpt-6-luna": {}, "gpt-6.1-sol": {},
	"gpt-6": {}, "gpt-5.6": {}, "gpt-5-mini": {}, "gpt-5-nano": {}, "gpt-5-pro": {},
	"gpt-5-codex": {}, "gpt-5.1-codex": {}, "gpt-5.1-codex-mini": {}, "gpt-5.1-codex-max": {},
	"gpt-5.2-codex": {}, "gpt-5.2-pro": {}, "gpt-5.4-pro": {},
}

func (s *OpenAIGatewayService) openAIWSRouteExceedsThreshold(finalModel string, sizes ...int) bool {
	if _, known := openAIWSGPTThresholdModels[strings.ToLower(strings.TrimSpace(finalModel))]; !known {
		return false
	}
	threshold := s.openAIWSHTTPBridgeThresholdBytes()
	for _, size := range sizes {
		if threshold > 0 && int64(size) >= threshold {
			return true
		}
	}
	return false
}

func (s *OpenAIGatewayService) shouldBridgeOpenAIWSHTTP(account *Account, payloadBytes int, previousResponseID string, finalModel ...string) bool {
	if account != nil && account.Platform == PlatformGrok {
		return true
	}
	if !s.openAIWSHTTPBridgeEnabled() {
		return false
	}
	if strings.TrimSpace(previousResponseID) != "" {
		return false
	}
	model := ""
	if len(finalModel) > 0 {
		model = finalModel[0]
	}
	return s.openAIWSRouteExceedsThreshold(model, payloadBytes)
}

func (s *OpenAIGatewayService) shouldBridgeOpenAIWSPassthroughFirstMessage(account *Account, payload []byte, finalModel ...string) bool {
	if account != nil && account.Platform == PlatformGrok {
		return true
	}
	if !s.openAIWSHTTPBridgeEnabled() {
		return false
	}
	model := strings.TrimSpace(gjson.GetBytes(payload, "model").String())
	if account != nil {
		model = normalizeOpenAIModelForUpstream(account, account.GetMappedModel(model))
	}
	if len(finalModel) > 0 {
		model = finalModel[0]
	}
	if !s.openAIWSRouteExceedsThreshold(model, len(payload)) {
		return false
	}
	if !json.Valid(payload) {
		return false
	}

	i := skipOpenAIWSJSONSpace(payload, 0)
	if i >= len(payload) || payload[i] != '{' {
		return false
	}
	i++
	eventType := "response.create"
	previousResponseID := ""
	typeSeen, previousResponseIDSeen := false, false
	for {
		i = skipOpenAIWSJSONSpace(payload, i)
		if payload[i] == '}' {
			break
		}
		keyStart := i
		keyEnd := scanOpenAIWSJSONString(payload, keyStart)
		i = skipOpenAIWSJSONSpace(payload, keyEnd)
		i++ // json.Valid guarantees the colon.
		i = skipOpenAIWSJSONSpace(payload, i)
		valueStart := i
		i = skipOpenAIWSJSONValue(payload, i)

		key := ""
		// A critical key is at most 20 decoded bytes. The generous encoded bound
		// covers escaped spellings without allocating attacker-sized key strings.
		if keyEnd-keyStart <= 128 {
			_ = json.Unmarshal(payload[keyStart:keyEnd], &key)
		}
		switch key {
		case "type":
			if typeSeen {
				return false
			}
			typeSeen = true
			var value *string
			if err := json.Unmarshal(payload[valueStart:i], &value); err != nil {
				return false
			}
			if value == nil || strings.TrimSpace(*value) == "" {
				eventType = "response.create"
			} else {
				eventType = strings.TrimSpace(*value)
			}
		case "previous_response_id":
			if previousResponseIDSeen {
				return false
			}
			previousResponseIDSeen = true
			var value *string
			if err := json.Unmarshal(payload[valueStart:i], &value); err != nil {
				return false
			}
			if value != nil {
				previousResponseID = strings.TrimSpace(*value)
			}
		}
		i = skipOpenAIWSJSONSpace(payload, i)
		if payload[i] == ',' {
			i++
		}
	}
	return eventType == "response.create" && previousResponseID == ""
}

func skipOpenAIWSJSONSpace(payload []byte, i int) int {
	for i < len(payload) {
		switch payload[i] {
		case ' ', '\t', '\r', '\n':
			i++
		default:
			return i
		}
	}
	return i
}

func scanOpenAIWSJSONString(payload []byte, i int) int {
	for i++; i < len(payload); i++ {
		switch payload[i] {
		case '\\':
			i++
		case '"':
			return i + 1
		}
	}
	return len(payload)
}

func skipOpenAIWSJSONValue(payload []byte, i int) int {
	if payload[i] == '"' {
		return scanOpenAIWSJSONString(payload, i)
	}
	if payload[i] != '{' && payload[i] != '[' {
		for i < len(payload) && payload[i] != ',' && payload[i] != '}' {
			i++
		}
		return i
	}
	depth := 0
	for ; i < len(payload); i++ {
		switch payload[i] {
		case '"':
			i = scanOpenAIWSJSONString(payload, i) - 1
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return len(payload)
}

func prepareOpenAIWSHTTPBridgeBody(account *Account, payload []byte) ([]byte, error) {
	object, _, err := parseOpenAIJSONObject(payload)
	if err != nil {
		return nil, err
	}
	if object == nil {
		return nil, errors.New("response.create payload must be a JSON object")
	}
	replacements := map[string][]byte{"stream": []byte("true")}
	deletions := map[string]bool{"type": true, "generate": true, "previous_response_id": true}
	if !shouldPreserveOpenAIResponsesNoneReasoningEffort(account) {
		if effort, _ := object.get("reasoning_effort"); effort.Type == gjson.String && strings.EqualFold(strings.TrimSpace(effort.String()), "none") {
			deletions["reasoning_effort"] = true
		}
		if reasoning, _ := object.get("reasoning"); reasoning.IsObject() {
			reasoningObject, _, parseErr := parseOpenAIJSONObject(openAIWSRawMessageFromResult(payload, reasoning))
			if parseErr != nil {
				return nil, parseErr
			}
			dropEffort := false
			if effort, _ := reasoningObject.get("effort"); effort.Type == gjson.String {
				dropEffort = strings.EqualFold(strings.TrimSpace(effort.String()), "none")
			}
			if len(reasoningObject.lastIndex) == 0 || (dropEffort && len(reasoningObject.lastIndex) == 1) {
				deletions["reasoning"] = true
			} else if dropEffort {
				replacements["reasoning"] = reasoningObject.rewrite(nil, map[string]bool{"effort": true}, nil)
			}
		}
	}
	// Preserve opaque input bytes and emit one final buffer. Decoding and
	// re-encoding input used several body-sized buffers on every bridge turn.
	return object.rewrite(replacements, deletions, []string{"stream"}), nil
}

type openAIWSToolCallReplayCollector struct {
	items    []json.RawMessage
	seen     map[string]struct{}
	allItems []json.RawMessage
	allSeen  map[string]struct{}
}

func (c *openAIWSToolCallReplayCollector) AddEvent(eventType string, message []byte) {
	switch strings.TrimSpace(eventType) {
	case "response.output_item.done":
		item := gjson.GetBytes(message, "item")
		c.addAllItem(item)
		c.addItem(item)
	case "response.completed", "response.done":
		output := gjson.GetBytes(message, "response.output")
		if !output.IsArray() {
			return
		}
		for _, item := range output.Array() {
			c.addAllItem(item)
			c.addItem(item)
		}
	}
}

// Items/AllItems 返回浅拷贝头数组；正文由 collector 独立分配且此后不可变，
// 调用方按 replay 所有权不变式共享持有。
func (c *openAIWSToolCallReplayCollector) Items() []json.RawMessage {
	return slices.Clone(c.items)
}

func (c *openAIWSToolCallReplayCollector) AllItems() []json.RawMessage {
	return slices.Clone(c.allItems)
}

func (c *openAIWSToolCallReplayCollector) addAllItem(item gjson.Result) {
	if !item.Exists() || item.Type != gjson.JSON {
		return
	}
	raw := strings.TrimSpace(item.Raw)
	if raw == "" || !strings.HasPrefix(raw, "{") || strings.TrimSpace(item.Get("type").String()) == "" {
		return
	}
	key := strings.TrimSpace(item.Get("id").String())
	if key == "" {
		key = strings.TrimSpace(item.Get("call_id").String())
	}
	if key == "" {
		key = raw
	}
	if c.allSeen == nil {
		c.allSeen = make(map[string]struct{})
	}
	if _, ok := c.allSeen[key]; ok {
		return
	}
	c.allSeen[key] = struct{}{}
	c.allItems = append(c.allItems, json.RawMessage(raw))
}

func (c *openAIWSToolCallReplayCollector) addItem(item gjson.Result) {
	if !item.Exists() || item.Type != gjson.JSON {
		return
	}
	raw := strings.TrimSpace(item.Raw)
	if raw == "" || !strings.HasPrefix(raw, "{") {
		return
	}
	if !isCodexToolCallContextItemType(item.Get("type").String()) {
		return
	}
	key := strings.TrimSpace(item.Get("id").String())
	if key == "" {
		key = strings.TrimSpace(item.Get("call_id").String())
	}
	if key == "" {
		key = raw
	}
	if c.seen == nil {
		c.seen = make(map[string]struct{})
	}
	if _, ok := c.seen[key]; ok {
		return
	}
	c.seen[key] = struct{}{}
	c.items = append(c.items, json.RawMessage(raw))
}

func buildOpenAIWSHTTPBridgeErrorEvent(statusCode int, message string) []byte {
	message = strings.TrimSpace(message)
	if message == "" {
		message = http.StatusText(statusCode)
	}
	if message == "" {
		message = "upstream request failed"
	}
	event := map[string]any{
		"type":            "error",
		"sequence_number": 0,
		"status":          statusCode,
		"error": map[string]any{
			"type":    "upstream_error",
			"message": message,
		},
	}
	body, err := json.Marshal(event)
	if err != nil {
		return []byte(`{"type":"error","sequence_number":0,"error":{"type":"upstream_error","message":"upstream request failed"}}`)
	}
	return body
}

func buildOpenAIWSHTTPBridgeFailedEvent(responseID, model string, source []byte, fallbackMessage string) []byte {
	errorType := strings.TrimSpace(gjson.GetBytes(source, "error.type").String())
	if errorType == "" {
		errorType = strings.TrimSpace(gjson.GetBytes(source, "response.error.type").String())
	}
	code := strings.TrimSpace(gjson.GetBytes(source, "error.code").String())
	if code == "" {
		code = strings.TrimSpace(gjson.GetBytes(source, "response.error.code").String())
	}
	if code == "" {
		code = "upstream_error"
	}
	message := extractOpenAISSEErrorMessage(source)
	if message == "" {
		message = strings.TrimSpace(fallbackMessage)
	}
	if message == "" {
		message = "Upstream response failed"
	}
	errorBody := map[string]any{"code": code, "message": message}
	if errorType != "" {
		errorBody["type"] = errorType
	}
	response := map[string]any{
		"id": responseID, "object": "response", "status": "failed",
		"output": []any{}, "error": errorBody,
	}
	if model = strings.TrimSpace(model); model != "" {
		response["model"] = model
	}
	body, err := json.Marshal(map[string]any{"type": "response.failed", "sequence_number": 0, "response": response})
	if err != nil {
		return []byte(`{"type":"response.failed","sequence_number":0,"response":{"status":"failed","output":[],"error":{"code":"upstream_error","message":"Upstream response failed"}}}`)
	}
	return body
}

func (s *OpenAIGatewayService) proxyOpenAIWSHTTPBridgeTurn(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	token string,
	payload []byte,
	payloadBytes int,
	originalModel string,
	imageBillingModel string,
	imageSizeTier string,
	imageInputSize string,
	grokCacheIdentity string,
	turn int,
	writeClientMessage func([]byte) error,
) (*OpenAIForwardResult, error) {
	if s == nil {
		return nil, errors.New("service is nil")
	}
	if s.httpUpstream == nil {
		return nil, errors.New("openai http upstream is nil")
	}
	if account == nil {
		return nil, errors.New("account is nil")
	}
	if writeClientMessage == nil {
		return nil, errors.New("client websocket writer is nil")
	}
	traffic := NewUsageTrafficCounter()
	ctx = WithUsageTrafficCounter(ctx, traffic)
	writeClientMessageWithTraffic := writeClientMessage
	writeClientMessage = func(message []byte) error {
		err := writeClientMessageWithTraffic(message)
		if err == nil {
			traffic.AddEgressBodyBytes(len(message))
		}
		return err
	}
	responseModelObserver := &upstreamResponseModelObserver{}

	body, err := prepareOpenAIWSHTTPBridgeBody(account, payload)
	if err != nil {
		return nil, fmt.Errorf("prepare http bridge body: %w", err)
	}
	// Only Grok uses this snapshot after adaptation. OpenAI bridge requests
	// do not need to retain another complete image context.
	var grokIntentSourceBody []byte
	if account.Platform == PlatformGrok {
		grokIntentSourceBody = append([]byte(nil), body...)
	}
	// Codex V2 分组策略：WS 入站 → HTTP bridge 的每一轮都按连接级固定的分组快照
	// 处理（决策在首次触达时固定，不逐帧重读）。父端移除 collaboration message 的
	// encrypted 标记，子端把明文 agent_message 换成普通 user 消息；无法确认明文时
	// 在向上游写入前 fail-closed。
	//
	// 续帧省略工具声明时，必须用连接保存的“客户端原始声明”重建本帧，再按本次实际
	// 选中的账号重新推导上游声明与回程标记：既不能让上一账号派生的形态（已去
	// encrypted 标记、已摊平）泄漏给另一个账号，也不能让客户端声明静默消失。
	// 两个载体（顶层 tools[] 与 Lite input[].additional_tools[]）同等对待；显式携带
	// 任意一个载体的帧都由本帧声明整体替换连接来源。
	functionToolUpstream := ((account.Platform == PlatformOpenAI || account.UsesNativeCNResponses() || account.ResponsesWSV2Profile() != "") && account.Type == AccountTypeAPIKey) || account.Platform == PlatformGrok
	policyEnabled := codexV2PolicyEnabledForRequest(c)
	inheritedState, _ := openAIWSHTTPBridgeToolStateFromContext(c)
	inheritedLoweredTools := decodeOpenAIWSHTTPBridgeLoweredTools(inheritedState.LoweredTools)

	frameTopLevelRaw, frameHasTopLevelTools := openAIWSHTTPBridgeRawField(body, "tools")
	frameAdditionalTools := codexV2ClientAdditionalToolsSource(body)
	frameDeclaredTools := frameHasTopLevelTools || len(frameAdditionalTools) > 0

	clientToolsSource := append(json.RawMessage(nil), inheritedState.ClientTools...)
	clientAdditionalToolsSource := decodeOpenAIWSHTTPBridgeRawItems(inheritedState.ClientAdditionalTools)
	if frameDeclaredTools {
		clientToolsSource = codexV2ClientToolsDeclarationSource(frameTopLevelRaw)
		clientAdditionalToolsSource = frameAdditionalTools
	}
	clientAdditionalToolsRaw := codexV2MarshalRawItems(clientAdditionalToolsSource)
	hasClientDeclaration := codexV2ToolsDeclarationHasEntries(clientToolsSource) ||
		len(clientAdditionalToolsSource) > 0
	hasDerivedDeclaration := codexV2ToolsDeclarationHasEntries(inheritedState.LoweredTools) ||
		codexV2ToolsDeclarationHasEntries(inheritedState.LoweredAdditionalTools) ||
		hasResponsesClientToolMapping(inheritedState.ClientMapping)

	// 启用组：续帧省略工具声明时按连接保存的客户端原始声明重建。
	//
	// 这里的判据只看“本连接是否启用策略”，不看 functionToolUpstream：后者只决定
	// 要不要把客户端工具降级成上游认识的 function 形态，而 OpenAI OAuth 的 pure
	// HTTP bridge 不做降级、却同样需要保存并替换客户端原始声明。把来源状态绑在
	// functionToolUpstream 上会让 OAuth 显式声明既不保存也不替换，后续省略帧只能
	// 重放更早一轮的陈旧声明。
	rehydratingClientTools := policyEnabled && !frameDeclaredTools && hasDerivedDeclaration
	if rehydratingClientTools {
		if !hasClientDeclaration {
			// 上一轮确实向上游声明过工具，但连接里没有可用的客户端原始声明来源，
			// 无法按本次账号安全重建。任何上游写入之前失败关闭，而不是发出一个
			// 去标记但回程不补 [] 的半套请求，也不是重放上一账号的陈旧声明。
			inheritanceErr := newCodexV2UnsafeToolInheritanceError()
			writeCodexV2WSBridgePolicyError(c, writeClientMessage, inheritanceErr)
			return nil, inheritanceErr
		}
		if codexV2ToolsDeclarationHasEntries(clientToolsSource) {
			rehydrated, rehydrateErr := sjson.SetRawBytes(body, "tools", clientToolsSource)
			if rehydrateErr != nil {
				return nil, fmt.Errorf("rehydrate codex v2 client tool declaration: %w", rehydrateErr)
			}
			body = rehydrated
		}
		if len(clientAdditionalToolsSource) > 0 {
			rehydrated, rehydrateErr := codexV2ReinjectAdditionalTools(body, clientAdditionalToolsSource)
			if rehydrateErr != nil {
				inheritanceErr := newCodexV2UnsafeToolInheritanceError()
				writeCodexV2WSBridgePolicyError(c, writeClientMessage, inheritanceErr)
				return nil, inheritanceErr
			}
			body = rehydrated
		}
	}
	policyOutcome, policyErr := applyCodexV2UpstreamRequestPoliciesForAttempt(c, account, body)
	if policyErr != nil {
		writeCodexV2WSBridgePolicyError(c, writeClientMessage, policyErr)
		return nil, policyErr
	}
	body = policyOutcome.Body
	_, grokExplicitToolsField := openAIWSHTTPBridgeRawField(grokIntentSourceBody, "tools")
	grokExplicitToolIntent := account.Platform == PlatformGrok && hasGrokResponsesToolIntent(grokIntentSourceBody)
	var clientToolMapping apicompat.ResponsesClientToolMapping
	if functionToolUpstream {
		if account.Platform == PlatformGrok {
			body, err = sanitizeGrokResponsesInput(body)
			if err != nil {
				return nil, fmt.Errorf("sanitize Grok WS HTTP bridge input: %w", err)
			}
		}
		// 启用组时连接声明已由本帧按实际账号重建（或保持客户端原样），适配器不再使用
		// 跨账号继承状态；关闭组保持既有继承行为不变。
		adapterInheritedMapping := inheritedState.ClientMapping
		adapterInheritedLoweredTools := inheritedLoweredTools
		if policyEnabled {
			adapterInheritedMapping = apicompat.ResponsesClientToolMapping{}
			adapterInheritedLoweredTools = nil
		}
		body, clientToolMapping, err = adaptResponsesClientToolsForFunctionUpstreamWithMapping(
			body,
			openAIWSHTTPBridgeToolUpstreamName(account),
			adapterInheritedMapping,
			adapterInheritedLoweredTools,
		)
		if err != nil {
			return nil, fmt.Errorf("adapt %s client tools: %w", openAIWSHTTPBridgeToolUpstreamName(account), err)
		}
		if account.Platform == PlatformGrok && !grokExplicitToolsField && !grokExplicitToolIntent && len(inheritedLoweredTools) > 0 && hasGrokResponsesToolIntent(body) {
			// This continuation omitted tools, so the pre-adapter source cannot
			// represent the effective inherited declarations. Cache routing must
			// see the rehydrated tool intent or it will replace client functions
			// with the native-search tool-free route. Explicit current-turn tool
			// intent still uses the original pre-sanitization source above.
			grokIntentSourceBody = append(grokIntentSourceBody[:0], body...)
		}
	}
	// 连接状态写入不以 functionToolUpstream 为前提：后者只决定要不要做 function-tool
	// 降级，而启用组的客户端原始声明与按账号派生结果在 OpenAI OAuth 的 pure HTTP
	// bridge 上同样需要保存（否则显式声明既不替换来源，续帧也只能重放更早的陈旧
	// 声明）。关闭组只有 functionToolUpstream 才写状态，与既有行为一致。
	if functionToolUpstream || policyEnabled {
		loweredTools := inheritedState.LoweredTools
		if currentTools, present := openAIWSHTTPBridgeRawField(body, "tools"); present {
			loweredTools = currentTools
		} else if policyEnabled {
			// 本帧出站没有顶层声明：启用组已按连接来源重建（或客户端本就没有），
			// 不允许把上一账号/上一轮的派生值留在状态里被后续重放。
			loweredTools = nil
		}
		loweredAdditionalTools := inheritedState.LoweredAdditionalTools
		if currentAdditionalTools := codexV2ClientAdditionalToolsSource(body); len(currentAdditionalTools) > 0 {
			loweredAdditionalTools = codexV2MarshalRawItems(currentAdditionalTools)
		} else if policyEnabled {
			loweredAdditionalTools = nil
		}
		// 标记与声明同源：ParentPlaintextSchema 与上面两个出站派生声明都来自本帧
		// 实际发给上游的同一份 body（策略改写后、适配器改写后）。
		setOpenAIWSHTTPBridgeToolState(c, openAIWSHTTPBridgeToolState{
			ClientMapping:                clientToolMapping,
			ClientTools:                  clientToolsSource,
			ClientAdditionalTools:        clientAdditionalToolsRaw,
			LoweredTools:                 loweredTools,
			LoweredAdditionalTools:       loweredAdditionalTools,
			CodexV2ParentPlaintextSchema: policyOutcome.ParentPlaintextSchema,
		})
	}
	if account.Platform != PlatformGrok && isOpenAIResponsesLiteWebSocketPayload(payload) {
		liteBody, liteChanged, liteErr := normalizeOpenAIResponsesLitePayloadForAccount(body, account)
		if liteErr != nil {
			return nil, fmt.Errorf("normalize responses Lite payload: %w", liteErr)
		}
		if liteChanged {
			body = liteBody
		}
	}

	buildUpstreamRequest := func(requestBody []byte) (*http.Request, error) {
		upstreamCtx, releaseUpstreamCtx := detachUpstreamContext(ctx)
		defer releaseUpstreamCtx()
		// Adaptive owns a downstream reader for the whole session. Keep its
		// cancellation on HTTP requests so disconnect/overlap releases large bodies.
		if s.IsOpenAIWSAdaptive(account) {
			upstreamCtx = ctx
		}
		var upstreamReq *http.Request
		var buildErr error
		if account.Platform == PlatformGrok {
			upstreamReq, buildErr = buildGrokResponsesRequest(upstreamCtx, c, account, requestBody, token, grokCacheIdentity, s.cfg, s.settingService)
		} else {
			upstreamReq, buildErr = s.buildUpstreamRequestOpenAIPassthrough(upstreamCtx, c, account, requestBody, token)
		}
		if buildErr != nil {
			return nil, buildErr
		}
		if account.Platform != PlatformGrok && isOpenAIResponsesLiteWebSocketPayload(payload) {
			upstreamReq.Header.Set(responsesLiteHeader, "true")
		}
		if err := applyMappedGPT55LiteCompatibility(upstreamReq, account, requestBody); err != nil {
			return nil, err
		}
		return upstreamReq, nil
	}
	if account.Platform == PlatformGrok {
		upstreamModel := resolveGrokWSUpstreamModel(account, body, originalModel)
		body, err = patchGrokResponsesBody(body, upstreamModel)
		if err != nil {
			return nil, err
		}
		grokMixedCacheIntentBody := append([]byte(nil), body...)
		body, err = applyGrokResponsesCacheIdentity(body, grokIntentSourceBody, grokCacheIdentity, account.IsGrokOAuth())
		if err != nil {
			return nil, fmt.Errorf("apply grok prompt cache identity: %w", err)
		}
		body, err = applyGrokFreeRequestToolCacheRoute(c, body, grokMixedCacheIntentBody, account, grokCacheIdentity)
		if err != nil {
			return nil, fmt.Errorf("apply grok Free function-tool cache route: %w", err)
		}
	}
	actualModel := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	if actualModel == "" {
		actualModel = canonicalOpenAIAccountSchedulingModel(account, originalModel)
	}
	SetOpsUpstreamModel(c, actualModel)

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	if c != nil {
		c.Set("openai_passthrough", true)
		c.Set("openai_ws_http_bridge", true)
	}

	turnStart := time.Now()
	rejectedFieldRetryState := newOpenAIResponsesRejectedFieldRetryState(body)
	var resp *http.Response
	for {
		upstreamReq, buildErr := buildUpstreamRequest(body)
		if buildErr != nil {
			return nil, buildErr
		}
		openAIWSCommitFromContext(ctx).advance(openAIWSTurnSentUnknown)
		resp, err = s.doOpenAIUpstream(upstreamReq, proxyURL, account)
		if err != nil {
			if openAIWSCommitFromContext(ctx) != nil {
				return nil, fmt.Errorf("upstream HTTP submission outcome unknown: %w", err)
			}
			if turn == 1 {
				return nil, s.handleOpenAIUpstreamTransportError(ctx, c, account, err, true)
			}
			safeErr := sanitizeUpstreamErrorMessage(err.Error())
			clientError := buildOpenAIWSHTTPBridgeErrorEvent(http.StatusBadGateway, "Upstream request failed")
			if writeErr := writeClientMessage(clientError); writeErr == nil {
				markOpenAIWSClientVisibleFailure(c, "error", clientError)
			}
			return nil, fmt.Errorf("upstream http bridge request failed: %s", safeErr)
		}
		if resp.StatusCode < 400 {
			break
		}

		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, openAIWSHTTPBridgeErrorBodyLimitBytes))
		_ = resp.Body.Close()
		markOpenAICyberPolicyEvent(c, respBody, resp.StatusCode, nil)
		if resp.StatusCode == http.StatusBadRequest &&
			extractUpstreamErrorCode(respBody) == openAIWSFallbackReasonInvalidEncryptedContent {
			s.markOpenAIWSInvalidEncryptedContentLineageFromPayload(
				c, body, "ingress_ws_http_bridge_invalid_encrypted_lineage_mark", account.ID, turn,
			)
		}
		retryBody, retryReason, changed, retryErr := normalizeOpenAIResponsesRejectedFieldRetryBody(resp.StatusCode, body, respBody)
		if retryErr != nil {
			return nil, fmt.Errorf("normalize websocket http bridge rejected field retry: %w", retryErr)
		}
		if changed && openAIWSCommitFromContext(ctx) == nil && rejectedFieldRetryState.Allow(retryBody) {
			logOpenAIWSModeInfo(
				"ingress_ws_http_bridge_rejected_field_retry account_id=%d turn=%d reason=%s",
				account.ID,
				turn,
				truncateOpenAIWSLogValue(retryReason, openAIWSLogValueMaxLen),
			)
			body = retryBody
			payloadBytes = len(body)
			continue
		}

		upstreamMsg := sanitizeUpstreamErrorMessage(strings.TrimSpace(extractUpstreamErrorMessage(respBody)))
		if upstreamMsg == "" {
			upstreamMsg = http.StatusText(resp.StatusCode)
		}
		shouldFailover := s.shouldFailoverOpenAIUpstreamResponse(account, resp.StatusCode, upstreamMsg, respBody)
		if account.Platform == PlatformGrok {
			shouldFailover = s.shouldFailoverGrokUpstreamError(resp.StatusCode, respBody)
			s.handleGrokAccountUpstreamError(withGrokTeamRateLimitModel(ctx, resolveGrokWSUpstreamModel(account, body, originalModel)), account, resp.StatusCode, resp.Header, respBody)
			if shouldFailover && (turn == 1 || resp.StatusCode == http.StatusTooManyRequests) {
				return nil, newOpenAIUpstreamFailoverError(resp.StatusCode, resp.Header, respBody, upstreamMsg, false)
			}
		} else if shouldFailover && (turn == 1 || resp.StatusCode == http.StatusTooManyRequests) {
			return nil, s.handleFailoverErrorResponsePassthrough(ctx, resp, c, account, body, respBody)
		}
		if account.Platform != PlatformGrok && (shouldFailover || shouldCooldownOpenAITransientUpstreamError(resp.StatusCode, respBody)) {
			s.handleOpenAIAccountUpstreamError(ctx, account, resp.StatusCode, resp.Header, respBody, actualModel)
		}
		clientError := buildOpenAIWSHTTPBridgeErrorEvent(resp.StatusCode, upstreamMsg)
		if openAIWSCommitFromContext(ctx) != nil {
			return nil, fmt.Errorf("upstream HTTP turn failed: status=%d", resp.StatusCode)
		}
		if writeErr := writeClientMessage(clientError); writeErr == nil {
			markOpenAIWSClientVisibleFailure(c, "error", clientError)
		}
		return nil, fmt.Errorf("upstream http bridge error: status=%d message=%s", resp.StatusCode, upstreamMsg)
	}
	defer func() { _ = resp.Body.Close() }()
	stopCancelBody := context.AfterFunc(ctx, func() { _ = resp.Body.Close() })
	defer stopCancelBody()
	if account.Platform == PlatformGrok {
		s.updateGrokUsageFromResponse(withGrokTeamRateLimitModel(ctx, resolveGrokWSUpstreamModel(account, body, originalModel)), account, resp.Header, resp.StatusCode)
	}

	responseID := ""
	usage := OpenAIUsage{}
	imageCounter := newOpenAIImageOutputCounter()
	var firstTokenMs *int
	reqStream := openAIWSPayloadBoolFromRaw(body, "stream", true)
	eventCount := 0
	tokenEventCount := 0
	terminalEventCount := 0
	replayCollector := &openAIWSToolCallReplayCollector{}
	firstEventType := ""
	lastEventType := ""
	upstreamTerminalEvent := ""
	sawDone := false
	wroteDownstream := false
	pendingClientMessages := make([][]byte, 0, 4)
	pendingClientMessageBytes := int64(0)
	capacityFailoverSuppressedLogged := false
	clientDisconnected := false
	officialOpenAIResponses := account != nil && account.Platform == PlatformOpenAI
	bareErrorPending := false
	var bareErrorPayload []byte
	bareErrorMessage := ""
	failureAccountSideEffectsApplied := false
	mappedModel := actualModel
	needModelReplace := false
	var mappedModelBytes []byte
	if originalModel != "" {
		needModelReplace = mappedModel != "" && mappedModel != originalModel
		if needModelReplace {
			mappedModelBytes = []byte(mappedModel)
		}
	}

	resultWithUsage := func() *OpenAIForwardResult {
		imageCount := imageCounter.Count()
		result := &OpenAIForwardResult{
			RequestID:                     responseID,
			Usage:                         usage,
			Model:                         originalModel,
			UpstreamModel:                 mappedModel,
			UpstreamResponseModel:         responseModelObserver.Model(),
			UpstreamResponseModelConflict: responseModelObserver.Conflict(),
			UpstreamResponseServiceTier:   responseModelObserver.ServiceTier(),
			ServiceTier:                   resolvedOpenAIUpstreamServiceTierFromObserver(responseModelObserver, extractOpenAIServiceTierFromBody(body)),
			ReasoningEffort:               ApplyThinkingEnabledFallback(extractOpenAIReasoningEffortFromBody(body, mappedModel, originalModel), body, mappedModel),
			RequestedReasoningEffort:      CanonicalRequestedReasoningEffort(body, originalModel, mappedModel),
			Stream:                        reqStream,
			OpenAIWSMode:                  true,
			OpenAIWSHTTPBridge:            true,
			UpstreamTerminalEvent:         upstreamTerminalEvent,
			ResponseHeaders:               cloneHeader(resp.Header),
			Duration:                      time.Since(turnStart),
			FirstTokenMs:                  firstTokenMs,
			Traffic:                       traffic.Snapshot(),
		}
		if replayInput := replayCollector.Items(); len(replayInput) > 0 {
			result.wsReplayInput = replayInput
			result.wsReplayInputExists = true
		}
		result.wsAccountFailoverReplayInput = replayCollector.AllItems()
		if imageCount > 0 {
			result.ImageCount = imageCount
			result.ImageSize = imageSizeTier
			result.ImageInputSize = imageInputSize
			result.ImageOutputSizes = imageCounter.Sizes()
			result.BillingModel = imageBillingModel
		}
		return result
	}

	maxLineSize := defaultMaxLineSize
	if s.cfg != nil && s.cfg.Gateway.MaxLineSize > 0 {
		maxLineSize = s.cfg.Gateway.MaxLineSize
	}
	if hasResponsesClientToolMapping(clientToolMapping) {
		resp.Body = newResponsesClientToolStreamBody(resp.Body, clientToolMapping, maxLineSize)
	}
	scanner := bufio.NewScanner(resp.Body)
	scanBuf := getSSEScannerBuf64K()
	scanner.Buffer(scanBuf[:0], maxLineSize)
	defer putSSEScannerBuf64K(scanBuf)

	pendingSSEEventType := ""
	finalizeBareError := func() error {
		if !bareErrorPending {
			return nil
		}
		if !failureAccountSideEffectsApplied {
			failureAccountSideEffectsApplied = s.handleOpenAIWSFailureAccountSideEffects(ctx, account, mappedModel, resp.Header, bareErrorPayload)
		}
		upstreamTerminalEvent = "response.failed"
		if clientDisconnected {
			return nil
		}
		clientMessage := buildOpenAIWSHTTPBridgeFailedEvent(responseID, originalModel, bareErrorPayload, bareErrorMessage)
		messages := append(pendingClientMessages, clientMessage)
		pendingClientMessages = nil
		pendingClientMessageBytes = 0
		for _, message := range messages {
			if err := writeClientMessage(message); err != nil {
				if isOpenAIWSClientDisconnectError(err) {
					clientDisconnected = true
					return nil
				}
				return fmt.Errorf("write synthesized websocket response.failed: %w", err)
			}
			wroteDownstream = true
		}
		markOpenAIWSClientVisibleFailure(c, "response.failed", clientMessage)
		return nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		if eventType, ok := extractOpenAISSEEventLine(line); ok {
			pendingSSEEventType = eventType
			continue
		}
		if strings.TrimSpace(line) == "" {
			pendingSSEEventType = ""
			continue
		}
		data, ok := extractOpenAISSEDataLine(line)
		if !ok {
			continue
		}
		trimmedData := strings.TrimSpace(data)
		if trimmedData == "" {
			continue
		}
		if trimmedData == "[DONE]" {
			sawDone = true
			continue
		}

		upstreamMessage := []byte(openAICompatPayloadWithEventType(trimmedData, pendingSSEEventType))
		if normalized, changed := normalizeCompletedImageGenerationStatus(upstreamMessage); changed {
			upstreamMessage = normalized
		}
		eventType, eventResponseID, _ := parseOpenAIWSEventEnvelope(upstreamMessage)
		responseModelObserver.ObserveOpenAI(upstreamMessage, eventType)
		if responseID == "" && eventResponseID != "" {
			responseID = eventResponseID
		}
		if eventType != "" {
			eventCount++
			if firstEventType == "" {
				firstEventType = eventType
			}
			lastEventType = eventType
		}
		if isOpenAIWSTokenEvent(eventType) {
			tokenEventCount++
			if firstTokenMs == nil {
				ms := int(time.Since(turnStart).Milliseconds())
				firstTokenMs = &ms
			}
		}
		if openAIWSMessageShouldParseUsage(eventType, upstreamMessage) {
			parseOpenAIWSResponseUsageFromCompletedEvent(upstreamMessage, &usage)
		}
		if openAIWSCommitFromContext(ctx) != nil && (eventType == "error" || eventType == "response.failed") {
			upstreamTerminalEvent = eventType
			s.handleOpenAIWSFailureAccountSideEffects(ctx, account, actualModel, resp.Header, upstreamMessage)
			markOpenAICyberPolicyEvent(c, upstreamMessage, http.StatusOK, &usage)
			if err := writeClientMessage(upstreamMessage); err != nil {
				return resultWithUsage(), err
			}
			markOpenAIWSClientVisibleFailure(c, eventType, upstreamMessage)
			return resultWithUsage(), errors.New("upstream HTTP stream failed after submission")
		}
		if eventType == "error" || eventType == "response.failed" {
			markOpenAICyberPolicyEvent(c, upstreamMessage, http.StatusOK, &usage)
		}
		imageCounter.AddSSEData(upstreamMessage)

		if needModelReplace && len(mappedModelBytes) > 0 && openAIWSEventMayContainModel(eventType) && strings.Contains(trimmedData, mappedModel) {
			upstreamMessage = replaceOpenAIWSMessageModel(upstreamMessage, mappedModel, originalModel)
		}
		if s.toolCorrector != nil && openAIWSEventMayContainToolCalls(eventType) && openAIWSMessageLikelyContainsToolCalls(upstreamMessage) {
			if corrected, changed := s.toolCorrector.CorrectToolCallsInSSEBytes(upstreamMessage); changed {
				upstreamMessage = corrected
			}
		}
		replayCollector.AddEvent(eventType, upstreamMessage)

		var upstreamEventErr error
		if officialOpenAIResponses && bareErrorPending && (eventType == "response.completed" || eventType == "response.done") {
			// Some upstreams emit a recoverable bare error before the authoritative
			// successful terminal. Do not replace that terminal with a synthetic
			// failure or retain side effects from the superseded error.
			bareErrorPending = false
			bareErrorPayload = nil
			bareErrorMessage = ""
		}
		suppressClientMessage := officialOpenAIResponses && bareErrorPending && eventType != "response.failed"
		if eventType == "error" || eventType == "response.failed" {
			errMessage := extractOpenAISSEErrorMessage(upstreamMessage)
			if errMessage == "" {
				errMessage = "upstream error event"
			}
			statusCode := openAIStreamFailureStatus(upstreamMessage, errMessage)
			shouldFailover := openAIStreamFailedEventShouldFailover(upstreamMessage, errMessage)
			if eventType == "error" {
				errCodeRaw, errTypeRaw, _ := parseOpenAIWSErrorEventFields(upstreamMessage)
				shouldFailover = openAIStreamErrorEventShouldFailover(upstreamMessage, errMessage)
				if account.Platform == PlatformGrok {
					statusCode = openAIWSErrorHTTPStatusFromRaw(errCodeRaw, errTypeRaw)
				}
				if reason, _ := classifyOpenAIWSErrorEventFromRaw(errCodeRaw, errTypeRaw, errMessage); reason == openAIWSFallbackReasonInvalidEncryptedContent {
					s.markOpenAIWSInvalidEncryptedContentLineageFromPayload(
						c, body, "ingress_ws_http_bridge_invalid_encrypted_lineage_mark", account.ID, turn,
					)
				}
			}
			requestScopedCapacity := isOpenAIUpstreamCapacityShedEvent(upstreamMessage)
			if account.Platform == PlatformGrok && eventType == "error" {
				// SSE error events do not carry an HTTP status. The local status
				// mapper therefore defaults unknown xAI codes (for example
				// new_sensitive) to 502; classify the body as a request-scoped
				// 403 before applying status-based failover or account state.
				if isGrokContentPolicyRejection(http.StatusForbidden, upstreamMessage) {
					shouldFailover = false
				} else {
					shouldFailover = s.shouldFailoverGrokUpstreamError(statusCode, upstreamMessage)
					s.handleGrokAccountUpstreamError(ctx, account, statusCode, resp.Header, upstreamMessage)
				}
			}
			// A disconnected client needs this attempt drained for usage, not replayed,
			// even when only non-semantic heartbeats were delivered.
			if !clientDisconnected && !wroteDownstream && shouldFailover && (turn == 1 || statusCode == http.StatusTooManyRequests) {
				if account.Platform == PlatformGrok {
					return nil, newOpenAIUpstreamFailoverError(statusCode, resp.Header, upstreamMessage, errMessage, false)
				}
				return nil, s.newOpenAIStreamFailoverErrorWithModel(c, account, true, resp.Header.Get("x-request-id"), upstreamMessage, errMessage, mappedModel, resp.Header)
			}
			if account.Platform != PlatformGrok && !failureAccountSideEffectsApplied {
				if eventType == "response.failed" || (!officialOpenAIResponses && shouldFailover && !requestScopedCapacity) {
					failureAccountSideEffectsApplied = s.handleOpenAIWSFailureAccountSideEffects(ctx, account, mappedModel, resp.Header, upstreamMessage)
				}
			}
			if wroteDownstream && requestScopedCapacity && !capacityFailoverSuppressedLogged {
				logOpenAICapacityFailoverSuppressed(ctx, account, "ws_http_bridge", resp.Header.Get("x-request-id"), eventType)
				capacityFailoverSuppressedLogged = true
			}
			if eventType == "error" && !officialOpenAIResponses {
				upstreamEventErr = errors.New(errMessage)
			} else if eventType == "error" {
				bareErrorPending = true
				bareErrorPayload = append(bareErrorPayload[:0], upstreamMessage...)
				bareErrorMessage = errMessage
				suppressClientMessage = true
			} else {
				bareErrorPending = false
			}
		}

		clientMessage := upstreamMessage
		// Codex V2 父端回程：仅在本次尝试改写过 collaboration schema 时补
		// encrypted_function_args: []，且只作用于完整明文调用项。
		clientMessage = applyCodexV2ParentResponsePolicy(c, clientMessage)
		if !clientDisconnected && !suppressClientMessage {
			isKeepalive := eventType == "keepalive"
			stageBeforeSemanticOutput := turn == 1 && account.Platform == PlatformOpenAI && !wroteDownstream
			commitStagedMessages := !stageBeforeSemanticOutput ||
				openAIStreamDataStartsClientOutput(string(clientMessage), eventType) ||
				isOpenAIWSTerminalEvent(eventType)
			if stageBeforeSemanticOutput && !commitStagedMessages && !isKeepalive {
				if pendingClientMessageBytes+int64(len(clientMessage)) > openAIFirstOutputStageMaxBytes {
					return nil, s.newOpenAIStreamFailoverError(
						c,
						account,
						true,
						resp.Header.Get("x-request-id"),
						nil,
						"OpenAI WS HTTP bridge first-output staging limit exceeded",
						resp.Header,
					)
				}
				pendingClientMessages = append(pendingClientMessages, append([]byte(nil), clientMessage...))
				pendingClientMessageBytes += int64(len(clientMessage))
			} else {
				// Keep the client connection alive without committing this attempt
				// or exposing its staged lifecycle metadata.
				var messages [][]byte
				if !isKeepalive {
					messages = pendingClientMessages
					pendingClientMessages = nil
					pendingClientMessageBytes = 0
				}
				messages = append(messages, clientMessage)
				for _, message := range messages {
					if err := writeClientMessage(message); err != nil {
						if isOpenAIWSClientDisconnectError(err) {
							clientDisconnected = true
							closeStatus, closeReason := summarizeOpenAIWSReadCloseError(err)
							logOpenAIWSModeInfo(
								"ingress_ws_http_bridge_client_disconnected_drain account_id=%d turn=%d close_status=%s close_reason=%s",
								account.ID,
								turn,
								closeStatus,
								truncateOpenAIWSLogValue(closeReason, openAIWSHeaderValueMaxLen),
							)
							break
						}
						return nil, wrapOpenAIWSIngressTurnError(
							"write_client",
							fmt.Errorf("write client websocket event: %w", err),
							wroteDownstream,
						)
					}
					if !isKeepalive {
						wroteDownstream = true
					}
				}
			}
		}
		if !clientDisconnected && !suppressClientMessage {
			markOpenAIWSClientVisibleFailure(c, eventType, upstreamMessage)
		}

		if upstreamEventErr != nil {
			return resultWithUsage(), upstreamEventErr
		}
		if isOpenAIWSTerminalEvent(eventType) && !bareErrorPending {
			if eventType == "response.failed" {
				upstreamTerminalEvent = "response.failed"
			} else {
				upstreamTerminalEvent = s.handleOpenAIWSTerminalTransientFailure(ctx, account, mappedModel, resp.Header, upstreamMessage)
			}
			terminalEventCount++
			firstTokenMsValue := -1
			if firstTokenMs != nil {
				firstTokenMsValue = *firstTokenMs
			}
			logOpenAIWSModeInfo(
				"ingress_ws_http_bridge_turn_completed account_id=%d turn=%d response_id=%s payload_bytes=%d duration_ms=%d events=%d token_events=%d terminal_events=%d first_event=%s last_event=%s first_token_ms=%d client_disconnected=%v",
				account.ID,
				turn,
				truncateOpenAIWSLogValue(responseID, openAIWSIDValueMaxLen),
				payloadBytes,
				time.Since(turnStart).Milliseconds(),
				eventCount,
				tokenEventCount,
				terminalEventCount,
				truncateOpenAIWSLogValue(firstEventType, openAIWSLogValueMaxLen),
				truncateOpenAIWSLogValue(lastEventType, openAIWSLogValueMaxLen),
				firstTokenMsValue,
				clientDisconnected,
			)
			return resultWithUsage(), nil
		}
	}
	if bareErrorPending {
		if finalizeErr := finalizeBareError(); finalizeErr != nil {
			return resultWithUsage(), finalizeErr
		}
		if scanErr := scanner.Err(); scanErr != nil {
			return resultWithUsage(), fmt.Errorf("read upstream http bridge stream after error event: %w", scanErr)
		}
		return resultWithUsage(), errors.New(bareErrorMessage)
	}
	if err := scanner.Err(); err != nil {
		streamErr := fmt.Errorf("read upstream http bridge stream: %w", err)
		if turn == 1 && !clientDisconnected && !wroteDownstream {
			return nil, s.handleOpenAIUpstreamTransportError(ctx, c, account, streamErr, true)
		}
		return resultWithUsage(), streamErr
	}
	terminalErr := errors.New("upstream http bridge stream ended before terminal event")
	if sawDone {
		terminalErr = errors.New("upstream http bridge stream sent [DONE] before terminal event")
	}
	if turn == 1 && !clientDisconnected && !wroteDownstream {
		return nil, s.handleOpenAIUpstreamTransportError(ctx, c, account, terminalErr, true)
	}
	return resultWithUsage(), terminalErr
}

func resolveGrokWSCacheIdentity(c *gin.Context, account *Account, seedPayload, currentPayload []byte, originalModel string) (string, error) {
	body, err := prepareOpenAIWSHTTPBridgeBody(account, seedPayload)
	if err != nil {
		return "", err
	}
	upstreamModel := resolveGrokWSUpstreamModel(account, currentPayload, originalModel)
	body, err = patchGrokResponsesBody(body, upstreamModel)
	if err != nil {
		return "", err
	}
	return resolveGrokCacheIdentity(c, body, "", upstreamModel), nil
}

func resolveGrokWSUpstreamModel(account *Account, body []byte, originalModel string) string {
	upstreamModel := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	originalModel = strings.TrimSpace(originalModel)
	// Shared ingress has already applied channel and account mappings when the
	// body model differs from the client-facing model. Only resolve from the
	// original model when the body still carries that original value.
	if account != nil && originalModel != "" && (upstreamModel == "" || upstreamModel == originalModel) {
		if mappedModel := normalizeOpenAIModelForUpstream(account, account.GetMappedModel(originalModel)); mappedModel != "" {
			upstreamModel = mappedModel
		}
	}
	if upstreamModel == "" {
		upstreamModel = grokDefaultResponsesModel
	}
	return upstreamModel
}
