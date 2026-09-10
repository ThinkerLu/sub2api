package service

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/tidwall/gjson"
)

// Raw views borrow immutable request bytes for synchronous use. Callers must
// rebuild into a new buffer; never mutate the source while a view is in use.
type openAIJSONMember struct {
	key   string
	value gjson.Result
}

type openAIJSONObject struct {
	body      []byte
	members   []openAIJSONMember
	lastIndex map[string]int
}

func parseOpenAIJSONObject(body []byte) (*openAIJSONObject, bool, error) {
	if !json.Valid(body) {
		return nil, false, fmt.Errorf("invalid JSON")
	}
	if bytes.Equal(bytes.TrimSpace(body), []byte("null")) {
		return nil, true, nil
	}
	root := parseRawJSONView(body)
	if !root.IsObject() {
		return nil, false, fmt.Errorf("expected JSON object")
	}
	object := &openAIJSONObject{
		body:      body,
		lastIndex: make(map[string]int),
	}
	root.ForEach(func(key, value gjson.Result) bool {
		name := key.String()
		object.lastIndex[name] = len(object.members)
		object.members = append(object.members, openAIJSONMember{key: name, value: value})
		return true
	})
	return object, false, nil
}

func (o *openAIJSONObject) get(key string) (gjson.Result, bool) {
	index, ok := o.lastIndex[key]
	if !ok {
		return gjson.Result{}, false
	}
	return o.members[index].value, true
}

func (o *openAIJSONObject) rewrite(replacements map[string][]byte, deletions map[string]bool, additionOrder []string) []byte {
	return o.rewriteWith(replacements, deletions, additionOrder, nil)
}

func (o *openAIJSONObject) rewriteWith(replacements map[string][]byte, deletions map[string]bool, additionOrder []string, writers map[string]func(*bytes.Buffer)) []byte {
	// encoding/json 解到 map 时重复顶层键采用最后一个值；保持同样语义，同时让
	// 未改字段（尤其 input）沿用原始 JSON，避免数字和大字符串往返解码。
	capHint := len(o.body) + 64
	for key, value := range replacements {
		if current, exists := o.get(key); exists {
			if growth := len(value) - len(current.Raw); growth > 0 {
				capHint += growth
			}
		} else {
			capHint += len(key) + len(value) + 4
		}
	}
	var rebuilt bytes.Buffer
	rebuilt.Grow(capHint)
	_ = rebuilt.WriteByte('{')
	first := true
	writeMember := func(key string, value []byte, rawValue string) {
		if !first {
			_ = rebuilt.WriteByte(',')
		}
		first = false
		encodedKey, _ := json.Marshal(key)
		_, _ = rebuilt.Write(encodedKey)
		_ = rebuilt.WriteByte(':')
		if write, ok := writers[key]; ok {
			write(&rebuilt)
		} else if value != nil {
			_, _ = rebuilt.Write(value)
		} else {
			_, _ = rebuilt.WriteString(rawValue)
		}
	}
	for index, member := range o.members {
		if o.lastIndex[member.key] != index || deletions[member.key] {
			continue
		}
		if replacement, ok := replacements[member.key]; ok {
			writeMember(member.key, replacement, "")
		} else {
			writeMember(member.key, nil, member.value.Raw)
		}
	}
	for _, key := range additionOrder {
		if _, exists := o.lastIndex[key]; exists || deletions[key] {
			continue
		}
		if replacement, ok := replacements[key]; ok {
			writeMember(key, replacement, "")
		}
	}
	_ = rebuilt.WriteByte('}')
	return rebuilt.Bytes()
}
