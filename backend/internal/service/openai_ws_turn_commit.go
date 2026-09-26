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

type openAIWSTurnCommit struct{ state atomic.Int32 }
type openAIWSTurnCommitKey struct{}

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
