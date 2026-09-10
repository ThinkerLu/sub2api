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

func TestOpenAIWSReplayPayloadPreservesInputSemantics(t *testing.T) {
	for _, raw := range []string{
		`{}`, ` { } `, `{"input":null}`, `{"in\u0070ut":[],"input":["later"]}`, `{"input":["first"],"input":["later"]}`, `{"model":"m"}`,
	} {
		payload := []byte(raw)
		original := bytes.Clone(payload)
		items := []json.RawMessage{nil, json.RawMessage(`{"text":"<>&","n":900719925474099312345}`)}
		got, err := setOpenAIWSPayloadInputSequence(payload, items, true)
		require.NoError(t, err)
		input, err := json.Marshal(items)
		require.NoError(t, err)
		want, err := sjson.SetRawBytes(payload, "input", input)
		require.NoError(t, err)
		// Compare through UseNumber so a large integer cannot hide precision loss.
		var gotObject, wantObject map[string]any
		require.NoError(t, decodeOpenAIJSONUseNumber(got, &gotObject))
		require.NoError(t, decodeOpenAIJSONUseNumber(want, &wantObject))
		require.Equal(t, wantObject, gotObject)
		require.Equal(t, original, payload)
	}
	for _, item := range []json.RawMessage{json.RawMessage{}, json.RawMessage(`{`)} {
		_, err := setOpenAIWSPayloadInputSequence([]byte(`{}`), []json.RawMessage{item}, true)
		require.Error(t, err)
	}
}
