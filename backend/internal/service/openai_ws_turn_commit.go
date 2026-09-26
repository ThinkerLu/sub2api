package service

import (
	"context"
	"sync/atomic"
)

type openAIWSTurnCommitState int32

const (
	openAIWSTurnNotSent openAIWSTurnCommitState = iota
	openAIWSTurnSentUnknown
	openAIWSTurnExecuting
	openAIWSTurnTerminal
)

type openAIWSTurnCommit struct {
	state atomic.Int32
	id    string
}
type openAIWSTurnCommitKey struct{}

func openAIWSUnknownExecutionEvent() []byte {
	return []byte(`{"type":"error","status":400,"error":{"type":"invalid_request_error","code":"upstream_execution_unknown","message":"Upstream execution could not be confirmed. This turn was not replayed; a new full-context request is required."}}`)
}

func openAIWSCommitFromContext(ctx context.Context) *openAIWSTurnCommit {
	if ctx == nil {
		return nil
	}
	c, _ := ctx.Value(openAIWSTurnCommitKey{}).(*openAIWSTurnCommit)
	return c
}

func (c *openAIWSTurnCommit) advance(next openAIWSTurnCommitState) {
	if c == nil {
		return
	}
	for old := c.state.Load(); old < int32(next); old = c.state.Load() {
		if c.state.CompareAndSwap(old, int32(next)) {
			return
		}
	}
}

func (c *openAIWSTurnCommit) notSent() bool {
	return c != nil && c.state.Load() == int32(openAIWSTurnNotSent)
}
