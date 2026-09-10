package service

import (
	"bytes"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestMemoryGrokPatchOwnsOutput(t *testing.T) {
	for _, model := range []string{"grok-4", "grok-4.5", "grok-4.20-beta"} {
		body := make([]byte, 0, 4096)
		body = append(body, []byte(`{"model":"grok-4","input":[{"role":"user","content":"hello"}],"metadata":{"x":1}}`)...)
		original := bytes.Clone(body)
		out, err := patchGrokResponsesBodyBase(body, model)
		require.NoError(t, err)
		require.Equal(t, original, body)
		clear(out)
		require.Equal(t, original, body, "output must not alias retry/billing request")
	}
}
