package service

import (
	"strings"
	"testing"
)

func BenchmarkMemoryPassthrough(b *testing.B) {
	payload := []byte(`{"type":"response.create","model":"gpt-5.5","input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,` + strings.Repeat("A", 8<<20) + `"}]}],"client_metadata":{"session_id":"s"},"prompt_cache_key":"s"}`)
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"chatgpt_account_id": "benchmark"}}
	b.Run("Identity8MiB", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			var err error
			memoryOptimizationSink, _, err = applyCodexAccountIdentityClientMetadataRaw(payload, account, 1)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("Compatibility8MiB", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			var err error
			memoryOptimizationSink, _, err = normalizeOpenAIResponsesWebSocketCompatibilityBody(payload, account, false)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
}
