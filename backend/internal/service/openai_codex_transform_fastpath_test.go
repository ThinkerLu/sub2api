package service

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCodexOAuthTransformFastPathNoOpLargeImage(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","instructions":"keep","stream":true,"store":false,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"describe"},{"type":"input_image","image_url":"data:image/png;base64,` + strings.Repeat("A", 8<<20) + `"}]}]}`)

	result, handled := tryCodexOAuthTransformFastPath(body, codexOAuthTransformOptions{IsCodexCLI: true})
	require.True(t, handled)
	require.Equal(t, "gpt-5.4", result.NormalizedModel)
	require.False(t, result.Modified)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(body, &decoded))
	mapResult := applyCodexOAuthTransformWithOptions(decoded, codexOAuthTransformOptions{IsCodexCLI: true})
	require.False(t, mapResult.Modified, "fast path must only handle proven map no-ops")
}

func TestCodexOAuthTransformFastPathFallsBackForInputRewrite(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "system role promotion",
			body: `{"model":"gpt-5.4","instructions":"keep","stream":true,"store":false,"input":[{"type":"message","role":"system","content":"system"}]}`,
		},
		{
			name: "invalid message id",
			body: `{"model":"gpt-5.4","instructions":"keep","stream":true,"store":false,"input":[{"type":"message","id":"item_bad","role":"user","content":"hello"}]}`,
		},
		{
			name: "legacy string input",
			body: `{"model":"gpt-5.4","instructions":"keep","stream":true,"store":false,"input":"hello"}`,
		},
		{
			name: "wrong store",
			body: `{"model":"gpt-5.4","instructions":"keep","stream":true,"store":true,"input":[]}`,
		},
		{
			name: "reserved tool name",
			body: `{"model":"gpt-5.4","instructions":"keep","stream":true,"store":false,"tools":[{"type":"function","name":"python"}],"input":[]}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, handled := tryCodexOAuthTransformFastPath([]byte(tt.body), codexOAuthTransformOptions{IsCodexCLI: true})
			require.False(t, handled)
		})
	}
}

func TestFilterCodexInputNoOpKeepsSlice(t *testing.T) {
	input := []any{map[string]any{
		"type": "message",
		"role": "user",
		"content": []any{
			map[string]any{"type": "input_text", "text": "hello"},
			map[string]any{"type": "input_image", "image_url": "data:image/png;base64,AAAA"},
		},
	}}

	filtered, changed := filterCodexInputWithOptionsResult(input, codexInputFilterOptions{PreserveReferences: true})
	require.False(t, changed)
	require.Len(t, filtered, 1)
	require.Same(t, &input[0], &filtered[0])
}
