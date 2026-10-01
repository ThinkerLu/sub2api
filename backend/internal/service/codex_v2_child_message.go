package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// rewriteCodexV2ChildAgentMessages 把发往非 OpenAI 上游的 agent_message 输入项
// 换成普通 user 消息。
//
// 规则（decision-1/decision-2）：
//   - 只处理完整、非空、全部片段均为明文的 agent_message；
//   - input_text/text 与 encrypted_content 片段按原信封顺序保留，未知片段、
//     空片段或真实密文一律拒绝（fail-closed），错误文本不包含正文；
//   - 其他输入项按原始字节原样保留。
//
// 返回值：改写后的 body、是否发生改写、错误。
func rewriteCodexV2ChildAgentMessages(body []byte) ([]byte, bool, error) {
	if len(body) == 0 ||
		!bytes.Contains(body, []byte("agent_message")) ||
		!json.Valid(body) {
		return body, false, nil
	}
	input := parseRawJSONView(body).Get("input")
	if !input.IsArray() {
		return body, false, nil
	}

	var rebuilt bytes.Buffer
	rebuilt.Grow(len(input.Raw) + 64)
	rebuilt.WriteByte('[')
	changed := false
	first := true
	var rewriteErr error
	input.ForEach(func(_, item gjson.Result) bool {
		if !first {
			rebuilt.WriteByte(',')
		}
		first = false
		if !item.IsObject() || strings.TrimSpace(item.Get("type").String()) != "agent_message" {
			_, _ = rebuilt.WriteString(item.Raw)
			return true
		}
		replacement, err := buildCodexV2UserMessageFromAgentMessage(item)
		if err != nil {
			rewriteErr = err
			return false
		}
		rebuilt.Write(replacement)
		changed = true
		return true
	})
	if rewriteErr != nil {
		return body, false, rewriteErr
	}
	if !changed {
		return body, false, nil
	}
	rebuilt.WriteByte(']')
	updated, err := sjson.SetRawBytes(body, "input", rebuilt.Bytes())
	if err != nil {
		return body, false, fmt.Errorf("rewrite codex v2 agent_message input: %w", err)
	}
	return updated, true, nil
}

// buildCodexV2UserMessageFromAgentMessage 按原片段顺序构造普通 user 消息。
// 无法确认明文时返回 codexV2ChildMessagePolicyError。
func buildCodexV2UserMessageFromAgentMessage(item gjson.Result) ([]byte, error) {
	content := item.Get("content")
	if content.Type == gjson.String {
		text := content.String()
		if strings.TrimSpace(text) == "" {
			return nil, newCodexV2ChildMessagePolicyError("content is empty or has an unsupported shape")
		}
		if !codexV2MessageLooksPlaintext(text) {
			return nil, newCodexV2ChildMessagePolicyError("content is encrypted or cannot be confirmed as plaintext")
		}
		return marshalCodexV2UserMessage([]any{map[string]any{"type": "input_text", "text": text}})
	}
	if !content.IsArray() {
		text := item.Get("text").String()
		if strings.TrimSpace(text) == "" {
			return nil, newCodexV2ChildMessagePolicyError("content is empty or has an unsupported shape")
		}
		if !codexV2MessageLooksPlaintext(text) {
			return nil, newCodexV2ChildMessagePolicyError("content is encrypted or cannot be confirmed as plaintext")
		}
		return marshalCodexV2UserMessage([]any{map[string]any{"type": "input_text", "text": text}})
	}
	rawParts := content.Array()
	if len(rawParts) == 0 {
		return nil, newCodexV2ChildMessagePolicyError("content is empty or has an unsupported shape")
	}
	parts := make([]any, 0, len(rawParts))
	var joined strings.Builder
	for _, part := range rawParts {
		switch strings.ToLower(strings.TrimSpace(part.Get("type").String())) {
		case "input_text", "text", "output_text":
			text := part.Get("text").String()
			if strings.TrimSpace(text) == "" {
				return nil, newCodexV2ChildMessagePolicyError("content contains an empty text part")
			}
			if !codexV2MessageLooksPlaintext(text) {
				return nil, newCodexV2ChildMessagePolicyError("content contains an encrypted text part")
			}
			joined.WriteString(text)
			parts = append(parts, map[string]any{"type": "input_text", "text": text})
		case "encrypted_content":
			text := part.Get("encrypted_content").String()
			if strings.TrimSpace(text) == "" {
				return nil, newCodexV2ChildMessagePolicyError("content contains an empty encrypted_content part")
			}
			if !codexV2MessageLooksPlaintext(text) {
				// 真密文：非 OpenAI 上游无法解密，禁止原样下发或当文本回显。
				return nil, newCodexV2ChildMessagePolicyError("content is real ciphertext the selected upstream cannot decode")
			}
			joined.WriteString(text)
			parts = append(parts, map[string]any{"type": "input_text", "text": text})
		default:
			return nil, newCodexV2ChildMessagePolicyError("content contains an unsupported part type")
		}
	}
	if strings.TrimSpace(joined.String()) == "" {
		return nil, newCodexV2ChildMessagePolicyError("content is empty or has an unsupported shape")
	}
	return marshalCodexV2UserMessage(parts)
}

func marshalCodexV2UserMessage(parts []any) ([]byte, error) {
	message := map[string]any{
		"type":    "message",
		"role":    "user",
		"content": parts,
	}
	encoded, err := json.Marshal(message)
	if err != nil {
		return nil, fmt.Errorf("encode codex v2 user message: %w", err)
	}
	return encoded, nil
}
