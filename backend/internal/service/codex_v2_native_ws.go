package service

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"

	openaiwsv2 "github.com/Wei-Shaw/sub2api/internal/service/openai_ws_v2"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Each ingress connection owns its source declarations. The response reader only
// reads the published marker; it never mutates gin or the request-side source.
type codexV2NativeWSPolicy struct {
	c       *gin.Context
	enabled bool
	mu      sync.Mutex
	active  atomic.Bool
}

func newCodexV2NativeWSPolicy(c *gin.Context) *codexV2NativeWSPolicy {
	return &codexV2NativeWSPolicy{c: c, enabled: codexV2PolicyEnabledForRequest(c)}
}

func (p *codexV2NativeWSPolicy) Prepare(payload []byte) ([]byte, error) {
	if p == nil || !p.enabled {
		return payload, nil
	}
	kind := gjson.GetBytes(payload, "type").String()
	if kind == "response.steer" {
		body, _, err := rewriteCodexV2ChildAgentMessages(payload)
		return body, err
	}
	if kind != "response.create" && kind != "session.update" {
		return payload, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	state, _ := openAIWSHTTPBridgeToolStateFromContext(p.c)
	top, topPresent := openAIWSHTTPBridgeRawField(payload, "tools")
	additional := codexV2ClientAdditionalToolsSource(payload)
	if topPresent || len(additional) > 0 {
		state.ClientTools = codexV2ClientToolsDeclarationSource(top)
		state.ClientAdditionalTools = codexV2MarshalRawItems(additional)
	} else {
		var err error
		if codexV2ToolsDeclarationHasEntries(state.ClientTools) {
			payload, err = sjson.SetRawBytes(payload, "tools", state.ClientTools)
			if err != nil {
				return nil, newCodexV2UnsafeToolInheritanceError()
			}
		}
		if saved := decodeOpenAIWSHTTPBridgeRawItems(state.ClientAdditionalTools); len(saved) > 0 {
			payload, err = codexV2ReinjectAdditionalTools(payload, saved)
			if err != nil {
				return nil, newCodexV2UnsafeToolInheritanceError()
			}
		}
	}
	body, _, err := rewriteCodexV2ChildAgentMessages(payload)
	if err != nil {
		return nil, err
	}
	body, applied, err := rewriteCodexV2ParentPlaintextToolSchema(body)
	if err != nil {
		return nil, err
	}
	state.LoweredTools, _ = openAIWSHTTPBridgeRawField(body, "tools")
	state.LoweredAdditionalTools = codexV2MarshalRawItems(codexV2ClientAdditionalToolsSource(body))
	if kind == "response.create" {
		state.CodexV2ParentPlaintextSchema = applied
		p.active.Store(applied)
	}
	// Shared source storage also makes adaptive WS -> HTTP transitions rebuild
	// from client declarations, not from the last upstream's alias schema.
	setOpenAIWSHTTPBridgeToolState(p.c, state)
	return body, nil
}

func (p *codexV2NativeWSPolicy) Restore(payload []byte) []byte {
	if p == nil || !p.enabled || !p.active.Load() || !json.Valid(payload) {
		return payload
	}
	if state, ok := openAIWSHTTPBridgeToolStateFromContext(p.c); ok && !state.CodexV2ParentPlaintextSchema {
		return payload
	}
	return insertCodexV2PlaintextFunctionCallMarkers(restoreCodexV2NamespaceCalls(payload))
}

type codexV2NativeWSFrameConn struct {
	inner  openaiwsv2.FrameConn
	policy *codexV2NativeWSPolicy
}

func (c *codexV2NativeWSFrameConn) ReadFrame(ctx context.Context) (coderws.MessageType, []byte, error) {
	return c.inner.ReadFrame(ctx)
}

func (c *codexV2NativeWSFrameConn) WriteFrame(ctx context.Context, kind coderws.MessageType, payload []byte) error {
	if kind == coderws.MessageText || kind == coderws.MessageBinary {
		var err error
		payload, err = c.policy.Prepare(payload)
		if err != nil {
			return NewOpenAIWSClientCloseError(coderws.StatusPolicyViolation, codexV2ClientPolicyErrorMessage(err), err)
		}
	}
	return c.inner.WriteFrame(ctx, kind, payload)
}

func (c *codexV2NativeWSFrameConn) Close() error { return c.inner.Close() }
