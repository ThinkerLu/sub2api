package service

import (
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
)

func sanitizeOpenAIResponsesKnownForeignReasoning(body []byte, finalModel string) ([]byte, bool, error) {
	passback := ResolveThinkingProtocol(strings.ToLower(strings.TrimSpace(finalModel))) == ThinkingProtocolPassbackRequired
	_, gpt := openAIWSGPTThresholdModels[strings.ToLower(strings.TrimSpace(finalModel))]
	if !passback && !gpt {
		return body, false, nil
	}
	shouldDrop := func(kind, encrypted string) bool {
		return kind == "reasoning" && ((passback && strings.HasPrefix(encrypted, "gAAAAA")) || (gpt && strings.HasSuffix(encrypted, "-0") && !strings.HasPrefix(encrypted, "gAAAAA")))
	}
	changed := false
	var profileErr error
	input := parseRawJSONView(body).Get("input")
	if !input.IsArray() {
		return body, false, nil
	}
	input.ForEach(func(_, item gjson.Result) bool {
		kind := item.Get("type").String()
		encrypted := item.Get("encrypted_content").String()
		if passback && (kind == "compaction" || kind == "compaction_summary") && strings.HasPrefix(encrypted, "gAAAAA") {
			profileErr = fmt.Errorf("target model cannot consume account-bound compaction history")
			return false
		}
		changed = changed || shouldDrop(kind, encrypted)
		return true
	})
	if profileErr != nil {
		return body, false, profileErr
	}
	if !changed {
		return body, false, nil
	}
	var request map[string]any
	if err := decodeOpenAIJSONUseNumber(body, &request); err != nil {
		return body, false, err
	}
	items, ok := request["input"].([]any)
	if !ok {
		return body, false, nil
	}
	filtered := make([]any, 0, len(items))
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			filtered = append(filtered, raw)
			continue
		}
		kind, _ := item["type"].(string)
		encrypted, _ := item["encrypted_content"].(string)
		if !shouldDrop(kind, encrypted) {
			filtered = append(filtered, item)
		}
	}
	request["input"] = filtered
	out, err := marshalOpenAIUpstreamJSON(request)
	if err != nil {
		return body, false, err
	}
	return out, true, nil
}
