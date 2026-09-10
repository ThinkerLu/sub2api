package service

import (
	"bytes"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"strings"
	"testing"
)

func BenchmarkMemoryOptimizationImageScan(b *testing.B) {
	payload := []byte(`{"input":[{"type":"input_image","image_url":"data:image/png;base64,` + strings.Repeat("A", 8<<20) + `"}]}`)
	b.ReportAllocs()
	for b.Loop() {
		memoryOptimizationBool = openAIRequestBodyMayContainImageInput(payload)
		memoryOptimizationBool = openAIRequestBodyMayContainEmptyBase64InputImage(payload)
	}
}

func TestMemoryImageScanMatchesLegacy(t *testing.T) {
	for _, raw := range []string{
		`{}`, `{"input":null}`, `{"input":[{"type":"input_image","image_url":"data:image/png;base64,"}]}`,
		`{"in\u0070ut":[{"type":"in\u0070ut_image","image_url":"data:image/png;base64,AA"}]}`,
		`{"messages":[{"content":[{"image_url":"url"}]}]}`, `{"input":[],"input":[{"image_url":"url"}]}`,
		`{"input":[{"content":[{"type":"input_text","text":"hello"}]}]}`,
	} {
		body := []byte(raw)
		original := bytes.Clone(body)
		want := openAIJSONValueMayContainImageInput(gjson.GetBytes(body, "input")) || openAIJSONValueMayContainImageInput(gjson.GetBytes(body, "messages.#-1"))
		require.Equal(t, want, openAIRequestBodyMayContainImageInput(body), raw)
		wantEmpty := openAIRequestBodyMayContainInputImageToken(body) && openAIJSONValueMayContainEmptyBase64InputImage(gjson.GetBytes(body, "input"))
		require.Equal(t, wantEmpty, openAIRequestBodyMayContainEmptyBase64InputImage(body), raw)
		require.Equal(t, original, body)
	}
}
