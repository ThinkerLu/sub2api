package service

import (
	"encoding/json"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

func TestMemoryWSRawFieldCompatibility(t *testing.T) {
	for _, body := range []string{`{"tools":[],"to\u006fls":null}`, `{"tools":[1,2],"input":"abc"}`, `null`, `[]`, `{"tools":[]} trailing`, `{"other":42}`, `{"tools":"\\u1234"}`} {
		var fields map[string]json.RawMessage
		err := json.Unmarshal([]byte(body), &fields)
		want, present := fields["tools"]
		if err != nil {
			want = nil
			present = false
		}
		got, ok := openAIWSHTTPBridgeRawField([]byte(body), "tools")
		require.Equal(t, present, ok, body)
		require.Equal(t, []byte(want), []byte(got), body)
	}
	body := []byte(`{"input":"` + strings.Repeat("x", 8<<20) + `","tools":[1]}`)
	got, ok := openAIWSHTTPBridgeRawField(body, "tools")
	require.True(t, ok)
	clear(body)
	require.Equal(t, "[1]", string(got))
}
