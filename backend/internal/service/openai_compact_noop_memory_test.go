package service

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestCompactionNoopFastPathBoundaries(t *testing.T) {
	for _, raw := range []string{`{"input":[{"type":"compaction_\u0074rigger"},{"role":"user","content":"x"}]}`, `{"input":[{"type":"compaction_trigger"},{"type":"compaction_trigger"}]}`} {
		_, changed, err := NormalizeCompactionTriggerInputOrder([]byte(raw))
		require.NoError(t, err)
		require.True(t, changed)
	}
	for _, raw := range []string{`{"input":[]}`, `null`, `{"input":[{"text":"hello"}]}`} {
		out, changed, err := NormalizeCompactionTriggerInputOrder([]byte(raw))
		require.NoError(t, err)
		require.False(t, changed)
		require.Equal(t, raw, string(out))
	}
	for _, raw := range []string{`{"input":`, `[]`, `"text"`} {
		_, _, err := NormalizeCompactionTriggerInputOrder([]byte(raw))
		require.Error(t, err)
	}
}
