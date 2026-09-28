package service

import (
	"bytes"
	"strings"

	"github.com/tidwall/gjson"
)

const openAIResponsesInputTextMaxChars = 10000000

// sanitizeOpenAIResponsesOrphanToolOutputsRaw removes orphan tool outputs
// without decoding the whole request into map[string]any. Large image history
// remains borrowed JSON fragments while only call/reference identifiers are
// scanned.
func sanitizeOpenAIResponsesOrphanToolOutputsRaw(body []byte) ([]byte, bool, error) {
	if len(body) == 0 {
		return body, false, nil
	}
	object, isNull, err := parseOpenAIJSONObject(body)
	if err != nil || isNull {
		return body, false, err
	}
	previous, _ := object.get("previous_response_id")
	if previous.Type == gjson.String && strings.TrimSpace(previous.String()) != "" {
		return body, false, nil
	}
	input, _ := object.get("input")
	if !input.IsArray() {
		return body, false, nil
	}

	toolCallIDs := make(map[string]struct{})
	referenceIDs := make(map[string]struct{})
	input.ForEach(func(_, item gjson.Result) bool {
		itemType := strings.TrimSpace(item.Get("type").String())
		if itemType == "item_reference" {
			if id := strings.TrimSpace(item.Get("id").String()); id != "" {
				referenceIDs[id] = struct{}{}
			}
			return true
		}
		if !isCodexToolCallContextItemType(itemType) {
			return true
		}
		id := strings.TrimSpace(item.Get("call_id").String())
		if id == "" {
			id = strings.TrimSpace(item.Get("id").String())
		}
		if id != "" {
			toolCallIDs[id] = struct{}{}
		}
		return true
	})

	isOrphan := func(item gjson.Result) bool {
		itemType := strings.TrimSpace(item.Get("type").String())
		if !isCodexToolCallOutputItemType(itemType) {
			return false
		}
		callID := strings.TrimSpace(item.Get("call_id").String())
		if callID == "" && itemType == "function_call_output" && strings.TrimSpace(item.Get("name").String()) != "" {
			return false
		}
		_, hasCall := toolCallIDs[callID]
		_, hasReference := referenceIDs[callID]
		return callID == "" || (!hasCall && !hasReference)
	}

	changed := false
	input.ForEach(func(_, item gjson.Result) bool {
		changed = isOrphan(item)
		return !changed
	})
	if !changed {
		return body, false, nil
	}
	result := object.rewriteWith(nil, nil, nil, map[string]func(*bytes.Buffer){"input": func(out *bytes.Buffer) {
		out.WriteByte('[')
		first := true
		input.ForEach(func(_, item gjson.Result) bool {
			if isOrphan(item) {
				return true
			}
			if !first {
				out.WriteByte(',')
			}
			first = false
			out.WriteString(item.Raw)
			return true
		})
		out.WriteByte(']')
	}})
	return result, true, nil
}

// sanitizeOpenAIResponsesOrphanToolOutputs removes tool-output items that have
// no matching call or item reference anywhere in the current input. Named
// function outputs without a call ID are standalone inputs, not orphan results.
func sanitizeOpenAIResponsesOrphanToolOutputs(reqBody map[string]any, input []any, hasPreviousResponseID bool) bool {
	if len(input) == 0 || hasPreviousResponseID {
		return false
	}

	toolCallIDs := make(map[string]struct{}, len(input))
	referenceIDs := make(map[string]struct{}, len(input))
	for _, rawItem := range input {
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		itemType := strings.TrimSpace(firstNonEmptyString(item["type"]))
		if itemType == "item_reference" {
			if id := strings.TrimSpace(firstNonEmptyString(item["id"])); id != "" {
				referenceIDs[id] = struct{}{}
			}
			continue
		}
		if !isCodexToolCallContextItemType(itemType) {
			continue
		}
		if id := strings.TrimSpace(firstNonEmptyString(item["call_id"], item["id"])); id != "" {
			toolCallIDs[id] = struct{}{}
		}
	}

	modified := false
	normalized := make([]any, 0, len(input))
	for _, rawItem := range input {
		item, ok := rawItem.(map[string]any)
		if !ok || !isCodexToolCallOutputItemType(strings.TrimSpace(firstNonEmptyString(item["type"]))) {
			normalized = append(normalized, rawItem)
			continue
		}

		callID := strings.TrimSpace(firstNonEmptyString(item["call_id"]))
		// Codex sends externally supplied inputs (such as task delegation) as
		// named function outputs without a preceding function call. Preserve
		// their native type, namespace and output instead of silently dropping
		// the request that started the turn.
		if isCodexStandaloneNamedFunctionOutput(
			firstNonEmptyString(item["type"]), callID, firstNonEmptyString(item["name"]),
		) {
			normalized = append(normalized, rawItem)
			continue
		}
		_, hasToolCall := toolCallIDs[callID]
		_, hasReference := referenceIDs[callID]
		if callID != "" && (hasToolCall || hasReference) {
			normalized = append(normalized, rawItem)
			continue
		}

		modified = true
	}
	if !modified {
		return false
	}
	reqBody["input"] = normalized
	return true
}

func truncateOpenAIResponsesInputText(_ map[string]any) bool {
	// Do not silently rewrite client or tool output. If an upstream enforces a
	// text limit, forwarding the original value preserves its explicit error for
	// the client and the normal Ops error pipeline. This compatibility shim is
	// retained until the two callers can remove the old mutation hook together.
	return false
}

func openAIResponsesInputMayNeedTruncation(_ []byte) bool {
	// See truncateOpenAIResponsesInputText. Returning false also avoids decoding
	// very large bodies solely for a mutation that must not happen.
	return false
}
