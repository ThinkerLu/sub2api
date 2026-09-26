package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

func openAIWSInputSuffixDigest(body []byte, count int) (string, int, error) {
	if hasDuplicateJSONObjectKeys(parseRawJSONView(body)) {
		return "", 0, ErrOpenAIWSRecoveryRejected
	}
	var request map[string]any
	if err := decodeOpenAIJSONUseNumber(body, &request); err != nil {
		return "", 0, err
	}
	items, ok := request["input"].([]any)
	if !ok || len(items) == 0 || count > len(items) {
		return "", 0, ErrOpenAIWSRecoveryRejected
	}
	if count <= 0 {
		count = len(items)
	}
	hash := sha256.New()
	encoder := json.NewEncoder(hash)
	for _, item := range items[len(items)-count:] {
		if err := encoder.Encode(item); err != nil {
			return "", 0, err
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), count, nil
}

func openAIWSPassbackFamily(model string) string {
	id := strings.ToLower(strings.TrimSpace(model))
	if ResolveThinkingProtocol(id) != ThinkingProtocolPassbackRequired {
		return ""
	}
	for _, prefix := range []string{"deepseek-", "glm-", "minimax-", "qwen"} {
		if strings.HasPrefix(id, prefix) {
			return prefix
		}
	}
	if strings.HasPrefix(id, "kimi-") || strings.HasPrefix(id, "moonshot-") || id == "k3" || id == "k3-256k" {
		return "kimi"
	}
	return ""
}

func openAIWSHistoryProtocol(model string) string {
	if _, gpt := openAIWSGPTThresholdModels[strings.ToLower(strings.TrimSpace(model))]; gpt {
		return "gpt"
	}
	return openAIWSPassbackFamily(model)
}

func openAIWSHistoryHasHostedReference(value any) bool {
	switch v := value.(type) {
	case map[string]any:
		for key, child := range v {
			if (key == "file_id" || key == "vector_store_id" || key == "container_id") && child != nil && child != "" {
				return true
			}
			if openAIWSHistoryHasHostedReference(child) {
				return true
			}
		}
	case []any:
		for _, child := range v {
			if openAIWSHistoryHasHostedReference(child) {
				return true
			}
		}
	}
	return false
}

// Migration builds a separate target copy. Account-bound opaque state cannot be
// translated into another provider's thinking or silently treated as messages.
func sanitizeOpenAIWSMigrationHistory(body []byte, sourceModel, targetModel string) ([]byte, error) {
	var request map[string]any
	if err := decodeOpenAIJSONUseNumber(body, &request); err != nil {
		return nil, err
	}
	if previous, exists := request["previous_response_id"]; exists && previous != nil && previous != "" {
		return nil, ErrOpenAIWSRecoveryRejected
	}
	items, ok := request["input"].([]any)
	if !ok || len(items) == 0 {
		return nil, ErrOpenAIWSRecoveryRejected
	}
	_, gpt := openAIWSGPTThresholdModels[strings.ToLower(strings.TrimSpace(targetModel))]
	targetFamily := openAIWSPassbackFamily(targetModel)
	if !gpt && targetFamily == "" {
		return nil, fmt.Errorf("migration target reasoning protocol unknown")
	}
	if openAIWSHistoryHasHostedReference(request) {
		return nil, fmt.Errorf("migration history contains account-bound hosted resources")
	}
	if tools, ok := request["tools"].([]any); ok {
		for _, raw := range tools {
			tool, ok := raw.(map[string]any)
			if !ok {
				return nil, ErrOpenAIWSRecoveryRejected
			}
			typ, _ := tool["type"].(string)
			if typ != "function" && typ != "custom" {
				return nil, fmt.Errorf("migration tool declaration is not portable")
			}
		}
	}
	filtered := make([]any, 0, len(items))
	calls := make(map[string]bool)
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			return nil, ErrOpenAIWSRecoveryRejected
		}
		typ, _ := item["type"].(string)
		switch typ {
		case "reasoning":
			encrypted, _ := item["encrypted_content"].(string)
			if gpt || targetFamily != openAIWSPassbackFamily(sourceModel) || strings.HasPrefix(encrypted, "gAAAAA") {
				continue
			}
		case "function_call", "custom_tool_call":
			id, _ := item["call_id"].(string)
			if id == "" {
				return nil, ErrOpenAIWSRecoveryRejected
			}
			if _, exists := calls[id]; exists {
				return nil, fmt.Errorf("duplicate migration tool call")
			}
			calls[id] = false
		case "function_call_output", "custom_tool_call_output":
			id, _ := item["call_id"].(string)
			done, exists := calls[id]
			if !exists || done {
				return nil, fmt.Errorf("migration tool output lacks a unique preceding call")
			}
			calls[id] = true
		case "message", "":
			role, _ := item["role"].(string)
			if role != "user" && role != "assistant" && role != "system" && role != "developer" {
				return nil, ErrOpenAIWSRecoveryRejected
			}
		default:
			return nil, fmt.Errorf("migration history item is not portable")
		}
		filtered = append(filtered, item)
	}
	for _, done := range calls {
		if !done {
			return nil, fmt.Errorf("migration tool call has no output")
		}
	}
	if len(filtered) == 0 {
		return nil, ErrOpenAIWSRecoveryRejected
	}
	request["input"] = filtered
	delete(request, "previous_response_id")
	return marshalOpenAIUpstreamJSON(request)
}
