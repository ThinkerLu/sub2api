package service

import (
	"bytes"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestImageCounterOwnsRetainedMetadata(t *testing.T) {
	counter := newOpenAIImageOutputCounter()
	for _, size := range []string{"", "1024x1024"} {
		body := []byte(`{"output":[{"type":"image_generation_call","id":"image-owned","result":"AAAA","size":"` + size + `"}]}`)
		counter.AddJSONResponse(body)
		for i := range body {
			body[i] = 'X'
		}
	}
	require.Equal(t, 1, counter.Count())
	require.Equal(t, []string{"1024x1024"}, counter.Sizes())
	data := []byte(`{"data":[{"url":"example","size":"1024x1024"}]}`)
	other := newOpenAIImageOutputCounter()
	other.AddJSONResponse(data)
	for i := range data {
		data[i] = 'X'
	}
	require.Equal(t, []string{"1024x1024"}, other.Sizes())
}

func BenchmarkImageCounterTextResponse(b *testing.B) {
	body := append([]byte(`{"output":[{"type":"message","content":[{"type":"output_text","text":"`), bytes.Repeat([]byte("A"), 8<<20)...)
	body = append(body, []byte(`"}]}]}`)...)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		countOpenAIResponseImageOutputsFromJSONBytes(body)
		collectOpenAIResponseImageOutputSizesFromJSONBytes(body)
	}
}
