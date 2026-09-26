package service

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestTargetReasoningDropsOnlyKnownForeignCipher(t *testing.T) {
	body := []byte(`{"input":[{"type":"reasoning","encrypted_content":"gAAAAA-native"},{"type":"reasoning","encrypted_content":"provider-placeholder-0","content":[{"type":"reasoning_text","text":"visible provider thinking"}]},{"role":"user","content":"continue"}]}`)
	for _, tc := range []struct{ target, first string }{{"deepseek-v4-pro", "provider-placeholder-0"}, {"gpt-5.1", "gAAAAA-native"}} {
		original := string(body)
		out, changed, err := sanitizeOpenAIResponsesKnownForeignReasoning(body, tc.target)
		require.NoError(t, err)
		require.True(t, changed)
		require.Equal(t, tc.first, gjson.GetBytes(out, "input.0.encrypted_content").String())
		require.Equal(t, "continue", gjson.GetBytes(out, "input.1.content").String())
		require.Equal(t, original, string(body))
		again, changed, err := sanitizeOpenAIResponsesKnownForeignReasoning(out, tc.target)
		require.NoError(t, err)
		require.False(t, changed)
		require.Equal(t, string(out), string(again))
	}
	out, changed, err := sanitizeOpenAIResponsesKnownForeignReasoning(body, "unknown-model")
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, string(body), string(out))
}

func TestTargetReasoningRejectsOpaqueCompactionForPassback(t *testing.T) {
	_, _, err := sanitizeOpenAIResponsesKnownForeignReasoning([]byte(`{"input":[{"type":"compaction","encrypted_content":"gAAAAA-old-context"}]}`), "deepseek-v4-pro")
	require.Error(t, err)
}
