package service

import (
	"bytes"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/sjson"
	"testing"
)

func TestReplaceOpenAIJSONFirstFieldValuesMatchesSJSON(t *testing.T) {
	for _, raw := range []string{
		`{"input":[{"text":"<>&"}],"client_metadata":{"session_id":"s"},"prompt_cache_key":"s"}`,
		` { "prompt_cache_key" : "s", "client_metadata" : {}, "prompt_cache_key" : "duplicate" } `,
		`{"client_metad\u0061ta":{},"client_metadata":{"keep":true},"prompt_cach\u0065_key":"s"}`,
	} {
		payload := []byte(raw)
		original := bytes.Clone(payload)
		replacements := map[string][]byte{"client_metadata": []byte(`{"session_id":"scoped"}`), "prompt_cache_key": []byte(`"scoped"`)}
		want, err := sjson.SetRawBytes(payload, "client_metadata", replacements["client_metadata"])
		require.NoError(t, err)
		want, err = sjson.SetRawBytes(want, "prompt_cache_key", replacements["prompt_cache_key"])
		require.NoError(t, err)
		got := replaceOpenAIJSONFirstFieldValues(payload, replacements)
		require.Equal(t, string(want), string(got))
		require.Equal(t, original, payload)
	}
}
