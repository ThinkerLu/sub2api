//go:build !race

package service

// Allocation budgets describe normal production builds. Race instrumentation
// changes allocation behavior; ownership and semantic tests remain enabled
// under -race in the companion test files.
import (
	"encoding/json"
	"github.com/stretchr/testify/require"
	"runtime"
	"strings"
	"testing"
)

func TestOpenAIWSReplayPayloadAllocation(t *testing.T) {
	item := json.RawMessage(`{"type":"input_text","text":"` + strings.Repeat("A", 8<<20) + `"}`)
	payload := []byte(`{"type":"response.create","model":"gpt-5.5","input":[],"previous_response_id":"resp_1"}`)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	result, err := setOpenAIWSPayloadInputSequence(payload, []json.RawMessage{item}, true)
	runtime.ReadMemStats(&after)
	require.NoError(t, err)
	require.True(t, json.Valid(result))
	require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(len(item)+(1<<20)), "history must be written directly to the final payload")
	runtime.KeepAlive(item)
}

func TestOpenAIWSReplayPrefixComparisonAllocation(t *testing.T) {
	large := json.RawMessage(`{"type":"input_image","image_url":"` + strings.Repeat("A", 8<<20) + `"}`)
	delta := json.RawMessage(`{"type":"input_image","image_url":"new"}`)
	reordered := json.RawMessage(`{"image_url":"` + strings.Repeat("A", 8<<20) + `","type":"input_image"}`)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	different := openAIWSRawItemsHasPrefix([]json.RawMessage{delta}, []json.RawMessage{large})
	equal := openAIWSRawItemsHasPrefix([]json.RawMessage{reordered}, []json.RawMessage{large})
	runtime.ReadMemStats(&after)
	require.False(t, different)
	require.True(t, equal)
	require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(1<<20), "prefix comparison must not decode and re-encode opaque input")
}

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
