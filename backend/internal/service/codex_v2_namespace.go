package service

import (
	"errors"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const codexV2UpstreamNamespace = "sub2api_collaboration"

var errCodexV2NamespaceCollision = errors.New("codex v2 group policy: upstream tool namespace collision")

// The alias avoids upstream reserved-schema validation. Client-side names stay unchanged.
func rewriteCodexV2NamespaceDeclarationsAndHistory(body []byte) ([]byte, bool, error) {
	root := parseRawJSONView(body)
	var names []string
	collision := false
	collect := func(base string, tools gjson.Result) {
		if !tools.IsArray() {
			return
		}
		for i, tool := range tools.Array() {
			if tool.Get("type").String() == "function" && strings.HasPrefix(tool.Get("name").String(), codexV2UpstreamNamespace+"__") {
				collision = true
			}
			if tool.Get("type").String() != "namespace" {
				continue
			}
			namespace := tool.Get("name").String()
			if namespace == codexV2UpstreamNamespace {
				collision = true
			}
			if namespace != codexV2CollaborationNamespace {
				continue
			}
			for _, child := range tool.Get("tools").Array() {
				if child.Get("type").String() == "function" && codexV2IsCollaborationMessageTool(child.Get("name").String()) {
					names = append(names, fmt.Sprintf("%s.%d.name", base, i))
					break
				}
			}
		}
	}
	collect("tools", root.Get("tools"))
	input := root.Get("input")
	for i, item := range input.Array() {
		if item.Get("type").String() == "additional_tools" {
			collect(fmt.Sprintf("input.%d.tools", i), item.Get("tools"))
		}
	}
	if collision {
		return body, false, errCodexV2NamespaceCollision
	}
	out := body
	for _, path := range names {
		var err error
		out, err = sjson.SetBytes(out, path, codexV2UpstreamNamespace)
		if err != nil {
			return body, false, err
		}
	}
	for i, item := range input.Array() {
		if item.Get("type").String() != "function_call" {
			continue
		}
		if item.Get("namespace").String() == codexV2UpstreamNamespace || strings.HasPrefix(item.Get("name").String(), codexV2UpstreamNamespace+"__") {
			return body, false, errCodexV2NamespaceCollision
		}
		base := fmt.Sprintf("input.%d", i)
		if item.Get("namespace").String() == codexV2CollaborationNamespace {
			var err error
			out, err = sjson.SetBytes(out, base+".namespace", codexV2UpstreamNamespace)
			if err != nil {
				return body, false, err
			}
		} else if item.Get("namespace").String() == "" && strings.HasPrefix(item.Get("name").String(), codexV2CollaborationNamespace+"__") {
			var err error
			name := codexV2UpstreamNamespace + strings.TrimPrefix(item.Get("name").String(), codexV2CollaborationNamespace)
			out, err = sjson.SetBytes(out, base+".name", name)
			if err != nil {
				return body, false, err
			}
		}
	}
	return out, len(names) > 0, nil
}

func restoreCodexV2NamespaceCalls(payload []byte) []byte {
	paths := collectCodexV2FunctionCallPaths(parseRawJSONView(payload), func(item gjson.Result) bool {
		return item.Get("type").String() == "function_call" &&
			(item.Get("namespace").String() == codexV2UpstreamNamespace ||
				item.Get("namespace").String() == "" && strings.HasPrefix(item.Get("name").String(), codexV2UpstreamNamespace+"__"))
	})
	out := payload
	for _, path := range paths {
		item := gjson.GetBytes(payload, path)
		if path == "" {
			item = parseRawJSONView(payload)
		}
		var err error
		out, err = sjson.SetBytes(out, codexV2JoinJSONPath(path, "namespace"), codexV2CollaborationNamespace)
		if err != nil {
			return payload
		}
		if item.Get("namespace").String() == "" {
			name := strings.TrimPrefix(item.Get("name").String(), codexV2UpstreamNamespace+"__")
			out, err = sjson.SetBytes(out, codexV2JoinJSONPath(path, "name"), name)
			if err != nil {
				return payload
			}
		}
	}
	return out
}
