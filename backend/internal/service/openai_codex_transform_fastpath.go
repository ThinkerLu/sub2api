package service

import (
	"strings"

	"github.com/tidwall/gjson"
)

// tryCodexOAuthTransformFastPath proves that applyCodexOAuthTransformWithOptions
// is a no-op for the current raw body. The predicate is intentionally
// conservative: any uncertain or legacy shape falls back to the full map path.
// This keeps request semantics unchanged while avoiding whole-body decode and
// encode for ordinary Responses messages, including large inline images.
func tryCodexOAuthTransformFastPath(body []byte, opts codexOAuthTransformOptions) (codexTransformResult, bool) {
	result := codexTransformResult{}
	if len(body) == 0 {
		return result, false
	}
	root := parseRawJSONView(body)
	if !root.IsObject() {
		return result, false
	}

	if root.Get("prompt").Exists() || root.Get("commands").Exists() {
		return result, false
	}
	if model := root.Get("model"); model.Type == gjson.String {
		normalized := strings.TrimSpace(model.String())
		if normalized != model.String() {
			return result, false
		}
		result.NormalizedModel = normalized
	}

	if opts.IsCompact {
		if root.Get("store").Exists() || root.Get("stream").Exists() {
			return result, false
		}
	} else if root.Get("store").Type != gjson.False || root.Get("stream").Type != gjson.True {
		return result, false
	}

	for _, field := range openAICodexOAuthUnsupportedFields {
		if root.Get(field).Exists() {
			return result, false
		}
	}
	if rawCodexReasoningIncludeNeedsChange(root) {
		return result, false
	}
	if root.Get("functions").Exists() || root.Get("function_call").Exists() {
		return result, false
	}
	if rawCodexToolsNeedNormalization(root.Get("tools")) {
		return result, false
	}
	if rawCodexToolChoiceNeedsNormalization(root.Get("tool_choice")) {
		return result, false
	}
	// Reserved tool-name aliasing is recursive and rare. Fall back on the raw
	// token instead of attempting a partial structural rewrite.
	if containsASCIIFold(body, []byte(codexReservedPythonToolName)) {
		return result, false
	}
	if promptCacheKey := root.Get("prompt_cache_key"); promptCacheKey.Type == gjson.String && isOpenAICompatMessagesBridgeBody(body) {
		return result, false
	}
	if !opts.SkipDefaultInstructions && !rawInstructionsNonEmpty(root.Get("instructions")) {
		return result, false
	}

	input := root.Get("input")
	if input.Exists() && !input.IsArray() {
		return result, false
	}
	if input.IsArray() && rawCodexInputNeedsTransformation(root, input, opts) {
		return result, false
	}

	result.PromptCacheKey = strings.TrimSpace(root.Get("prompt_cache_key").String())
	return result, true
}

func rawInstructionsNonEmpty(instructions gjson.Result) bool {
	return instructions.Type == gjson.String && strings.TrimSpace(instructions.String()) != ""
}

func rawCodexReasoningIncludeNeedsChange(root gjson.Result) bool {
	reasoning := root.Get("reasoning")
	if !reasoning.IsObject() || len(reasoning.Map()) == 0 {
		return false
	}
	include := root.Get("include")
	if !include.Exists() || include.Type == gjson.Null {
		return true
	}
	if !include.IsArray() {
		// ensureCodexReasoningInclude intentionally leaves unexpected types alone.
		return false
	}
	for _, item := range include.Array() {
		if item.Type == gjson.String && item.String() == "reasoning.encrypted_content" {
			return false
		}
	}
	return true
}

func rawCodexToolsNeedNormalization(tools gjson.Result) bool {
	if !tools.Exists() || tools.Type == gjson.Null || !tools.IsArray() {
		return false
	}
	for _, tool := range tools.Array() {
		if !tool.IsObject() || strings.TrimSpace(tool.Get("type").String()) != "function" {
			continue
		}
		if strings.TrimSpace(tool.Get("name").String()) == "" {
			return true
		}
	}
	return false
}

func rawCodexToolChoiceNeedsNormalization(choice gjson.Result) bool {
	if !choice.Exists() || choice.Type == gjson.Null || !choice.IsObject() {
		return false
	}
	return strings.TrimSpace(choice.Get("type").String()) != "allowed_tools"
}

func rawCodexInputNeedsTransformation(root, input gjson.Result, opts codexOAuthTransformOptions) bool {
	preserveReferences := rawNeedsToolContinuation(root, input)
	for _, item := range input.Array() {
		if !item.IsObject() {
			continue
		}
		role := strings.TrimSpace(item.Get("role").String())
		if role == "system" || role == "tool" {
			return true
		}
		typ := strings.TrimSpace(item.Get("type").String())
		if typ == "message" {
			if rawMessageContentNeedsTransformation(item.Get("content")) {
				return true
			}
		}
		if typ == "reasoning" {
			summary := item.Get("summary")
			if item.Get("id").Exists() || item.Get("call_id").Exists() || !summary.Exists() || summary.Type == gjson.Null {
				return true
			}
		}
		if typ == "item_reference" {
			if !preserveReferences {
				return true
			}
			if id := item.Get("id"); id.Type == gjson.String && strings.HasPrefix(strings.TrimSpace(id.String()), "call_") {
				return true
			}
		}
		if isCodexToolCallItemType(typ) {
			callID := strings.TrimSpace(item.Get("call_id").String())
			if callID == "" && strings.TrimSpace(item.Get("id").String()) != "" {
				return true
			}
			if callID != "" && normalizeCodexFilterCallID(typ, callID, opts.PreserveToolCallIDs) != callID {
				return true
			}
		} else if item.Get("call_id").Exists() {
			return true
		}
		if codexInputItemRequiresName(typ) && strings.TrimSpace(item.Get("name").String()) == "" {
			return true
		}
		if id := item.Get("id"); id.Exists() {
			if !preserveReferences {
				return true
			}
			if id.Type == gjson.String && shouldStripOpenAIResponsesInputItemID(typ, id.String()) {
				return true
			}
		}
	}
	return false
}

func rawMessageContentNeedsTransformation(content gjson.Result) bool {
	if !content.IsArray() {
		return false
	}
	for _, part := range content.Array() {
		if !part.IsObject() {
			continue
		}
		if text := part.Get("text"); text.Exists() && text.Type != gjson.String {
			return true
		}
	}
	return false
}

func rawNeedsToolContinuation(root, input gjson.Result) bool {
	if previous := root.Get("previous_response_id"); previous.Type == gjson.String && strings.TrimSpace(previous.String()) != "" {
		return true
	}
	if tools := root.Get("tools"); tools.IsArray() && len(tools.Array()) > 0 {
		return true
	}
	if choice := root.Get("tool_choice"); (choice.Type == gjson.String && strings.TrimSpace(choice.String()) != "") || (choice.IsObject() && len(choice.Map()) > 0) {
		return true
	}
	for _, item := range input.Array() {
		if !item.IsObject() {
			continue
		}
		typ := strings.TrimSpace(item.Get("type").String())
		if isCodexToolCallItemType(typ) || typ == "item_reference" {
			return true
		}
	}
	return false
}
