package service

import (
	"bytes"
	"encoding/json"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestOpenAIWSReplayPrefixComparisonMatchesLegacy(t *testing.T) {
	values := []string{
		`null`, `true`, `false`, `0`, `-0`, `1`, `1.0`, `1e0`, `900719925474099312345`, `900719925474099312346`,
		`"a"`, `"\u0061"`, `"a/b"`, `"a\/b"`, `"<>&"`, `"\u003c\u003e\u0026"`, `"\ud800"`, `"�"`,
		`[]`, `[null]`, `[1,2]`, `[2,1]`, `{}`, `{"a":1,"b":2}`, `{"b":2,"a":1}`, `{"a":0,"a":1}`, `{"a":1}`, `{"\u0061":1}`,
		`{"a":{"b":[1,null,"\u0061"]}}`, `{"a":{"b":[1,null,"a"]}}`,
		`{"a":`, `  {"a": `, ``, `"` + string([]byte{0xff}) + `"`,
	}
	for _, a := range values {
		for _, b := range values {
			want := bytes.Equal(normalizeOpenAIWSJSONForCompareOrRaw([]byte(a)), normalizeOpenAIWSJSONForCompareOrRaw([]byte(b)))
			got := openAIWSRawItemsHasPrefix([]json.RawMessage{json.RawMessage(a)}, []json.RawMessage{json.RawMessage(b)})
			require.Equal(t, want, got, "compare %q / %q", a, b)
		}
	}
}
