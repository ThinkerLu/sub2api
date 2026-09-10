package service

import (
	"encoding/json"
	"github.com/tidwall/gjson"
	"strings"
)

// These synchronous, immutable scans are conservative: a possible rewrite
// always falls back to the established decoder and its compatibility rules.
func openAIResponsesHasNoLegacyAliases(body []byte) bool {
	if !json.Valid(body) {
		return false
	}
	root := parseRawJSONView(body)
	if !root.IsObject() {
		return false
	}
	clean := true
	root.ForEach(func(key, value gjson.Result) bool {
		switch key.String() {
		case "messages", "commands":
			clean = false
		case "prompt":
			if value.Type == gjson.String {
				clean = false
			}
		}
		return clean
	})
	return clean
}

func openAIResponsesMayHaveOrphanToolOutputs(body []byte) bool {
	root := parseRawJSONView(body)
	var input, previous gjson.Result
	// The downstream encoding/json map uses last-key semantics, including
	// escaped keys. Scanning every member avoids gjson.Get's first-key rule.
	root.ForEach(func(key, value gjson.Result) bool {
		switch key.String() {
		case "input":
			input = value
		case "previous_response_id":
			previous = value
		}
		return true
	})
	if previous.Type == gjson.String && strings.TrimSpace(previous.Str) != "" {
		return false
	}
	if !input.IsArray() {
		return false
	}
	possible := false
	input.ForEach(func(_, item gjson.Result) bool {
		if item.IsObject() {
			item.ForEach(func(key, value gjson.Result) bool {
				if key.String() == "type" && value.Type == gjson.String && isCodexToolCallOutputItemType(strings.TrimSpace(value.Str)) {
					possible = true
				}
				return !possible
			})
		}
		return !possible
	})
	return possible
}
