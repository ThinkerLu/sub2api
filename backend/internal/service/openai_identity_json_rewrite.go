package service

import (
	"bytes"
	"github.com/tidwall/gjson"
)

// Borrow immutable JSON views and rebuild all identity fields in one buffer.
func replaceOpenAIJSONFirstFieldValues(body []byte, replacements map[string][]byte) []byte {
	type patch struct {
		value gjson.Result
		raw   []byte
	}
	var patches []patch
	seen := make(map[string]bool, len(replacements))
	size := len(body)
	parseRawJSONView(body).ForEach(func(key, value gjson.Result) bool {
		name := key.String()
		if raw, exists := replacements[name]; exists && !seen[name] {
			seen[name] = true
			patches = append(patches, patch{value, raw})
			size += len(raw) - len(value.Raw)
		}
		return true
	})
	if len(patches) == 0 {
		return body
	}
	var out bytes.Buffer
	out.Grow(size)
	start := 0
	for _, patch := range patches {
		out.Write(body[start:patch.value.Index])
		out.Write(patch.raw)
		start = patch.value.Index + len(patch.value.Raw)
	}
	out.Write(body[start:])
	return out.Bytes()
}
