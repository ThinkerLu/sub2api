package service

import (
	"bytes"
	"encoding/json"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestBridgeRawRewriteMatchesDecodedSemantics(t *testing.T) {
	for _, raw := range []string{
		`{"type":"response.create","stream":false,"stream":null,"input":[{"n":9007199254740993,"x":1,"x":2}],"reasoning":{"effort":"none","summary":"auto"}}`,
		`{"str\\eam":1,"str\u0065am":false,"reasoning":{"effort":"low","effort":"NONE"},"previous_response_id":"old","generate":false}`,
		`{"input":[],"reasoning":{},"reasoning_effort":"none"}`,
	} {
		body := []byte(raw)
		before := bytes.Clone(body)
		var expected map[string]any
		require.NoError(t, decodeOpenAIJSONUseNumber(body, &expected))
		delete(expected, "type")
		delete(expected, "generate")
		delete(expected, "previous_response_id")
		deleteOpenAIResponsesNoneReasoningEffortFromObject(nil, expected)
		expected["stream"] = true
		got, err := prepareOpenAIWSHTTPBridgeBody(nil, body)
		require.NoError(t, err)
		var actual map[string]any
		require.NoError(t, decodeOpenAIJSONUseNumber(got, &actual))
		require.Equal(t, expected, actual)
		require.Equal(t, before, body)
		got[0] = ' '
		require.Equal(t, before, body)
	}
	for _, raw := range []string{`null`, `[]`, `{"input":`, `{"input":[]} trailing`} {
		_, err := prepareOpenAIWSHTTPBridgeBody(nil, []byte(raw))
		require.Error(t, err)
	}
}

func BenchmarkBridgeImageBody(b *testing.B) {
	body, _ := json.Marshal(map[string]any{"type": "response.create", "input": string(bytes.Repeat([]byte("A"), 8<<20))})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := prepareOpenAIWSHTTPBridgeBody(nil, body); err != nil {
			b.Fatal(err)
		}
	}
}
