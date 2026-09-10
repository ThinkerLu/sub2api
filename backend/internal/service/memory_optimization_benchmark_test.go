package service

import (
	"encoding/json"
	"strings"
	"testing"
)

var memoryOptimizationSink []byte
var memoryOptimizationBool bool

func BenchmarkMemoryOptimization(b *testing.B) {
	large := strings.Repeat("A", 8<<20)
	payload := []byte(`{"model":"grok-4","input":[{"role":"user","content":"` + large + `"}],"tools":[{"type":"function","name":"f"}]}`)
	item := json.RawMessage(`{"role":"user","content":"` + large + `"}`)
	reordered := json.RawMessage(`{"content":"` + large + `","role":"user"}`)
	b.Run("WSRawField8MiB", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			memoryOptimizationSink, memoryOptimizationBool = openAIWSHTTPBridgeRawField(payload, "tools")
		}
	})
	b.Run("WSReplayWrite8MiB", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			var err error
			memoryOptimizationSink, err = setOpenAIWSPayloadInputSequence([]byte(`{"model":"gpt-5","input":[]}`), []json.RawMessage{item}, true)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("WSReplayCompare8MiB", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			memoryOptimizationBool = openAIWSRawItemsHasPrefix([]json.RawMessage{reordered}, []json.RawMessage{item})
		}
	})
	b.Run("HTTPGrokPatch8MiB", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			var err error
			memoryOptimizationSink, err = patchGrokResponsesBodyBase(payload, "grok-4")
			if err != nil {
				b.Fatal(err)
			}
		}
	})
}
