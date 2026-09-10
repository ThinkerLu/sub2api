package service

import (
	"bytes"
	"encoding/json"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestPassthroughOrphanScanConservative(t *testing.T) {
	for _, raw := range []string{
		`{"input":[{"type":"function_call_output","call_id":"orphan","output":"x"}]}`,
		`{"input":[{"type":"custom_tool_call_output","call_id":"orphan"}],"previous_response_id":"r"}`,
		`{"input":[{"type":"function_call_output","call_id":"orphan"}],"previous_response_id":"r","previous_response_id":""}`,
		`{"input":[],"in\u0070ut":[{"ty\u0070e":"function_call_output","call_id":"orphan"}]}`,
		`{"input":[{"type":"message","type":"function_call_output","call_id":"orphan"}]}`,
		`{"input":[{"role":"user","content":"hello"}]}`, `{"input":null}`,
	} {
		var decoded map[string]any
		require.NoError(t, json.Unmarshal([]byte(raw), &decoded))
		input, _ := decoded["input"].([]any)
		changed := sanitizeOpenAIResponsesOrphanToolOutputs(decoded, input, firstNonEmptyString(decoded["previous_response_id"]) != "")
		if !openAIResponsesMayHaveOrphanToolOutputs([]byte(raw)) {
			require.False(t, changed, raw)
		}
	}
}
func TestPassthroughNativeLegacyNoop(t *testing.T) {
	for _, raw := range []string{`{"input":[{"text":"hello","n":9007199254740993}]}`, `{"prompt":{"id":"p"},"input":null}`} {
		body := []byte(raw)
		original := bytes.Clone(body)
		out, changed, err := normalizeOpenAIResponsesLegacyIngress(body)
		require.NoError(t, err)
		require.False(t, changed)
		require.Equal(t, original, out)
		require.Equal(t, &body[0], &out[0])
	}
	for _, raw := range []string{`{"messages":null}`, `{"commands":[]}`, `{"prompt":"alias"}`, `{"prompt":{},"pro\u006dpt":"alias"}`, `{"input":[]} garbage`, `[]`} {
		require.False(t, openAIResponsesHasNoLegacyAliases([]byte(raw)), raw)
	}
}
