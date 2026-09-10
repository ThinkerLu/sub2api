package service

import (
	"bytes"
	"encoding/json"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/sjson"
	"runtime"
	"strings"
	"testing"
)

func TestCodexAccountIdentityCombinedRewriteAllocation(t *testing.T) {
	payload := []byte(`{"input":"` + strings.Repeat("A", 8<<20) + `","client_metadata":{"session_id":"s"},"prompt_cache_key":"s"}`)
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"chatgpt_account_id": "diagnostic-account"}}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	body, changed, err := applyCodexAccountIdentityClientMetadataRaw(payload, account, 1)
	runtime.ReadMemStats(&after)
	require.NoError(t, err)
	require.True(t, changed)
	require.True(t, json.Valid(body))
	require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(len(payload)+(1<<20)), "identity fields must be rewritten in one output buffer")
}

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
