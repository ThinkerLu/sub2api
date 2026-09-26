package service

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	openaiwsv2 "github.com/Wei-Shaw/sub2api/internal/service/openai_ws_v2"
	coderws "github.com/coder/websocket"
	"github.com/tidwall/gjson"
)

// The session owns the only transport reader. Turn cancellation stops a queue
// consumer, never the underlying ReadFrame that also answers upstream pings.
type openAIWSAdaptiveRetainedConn struct {
	openAIWSClientConn
	frames openaiwsv2.FrameConn
	ctx    context.Context
	cancel context.CancelFunc
	queue  chan openAIWSAdaptiveFrame
	done   chan struct{}
	once   sync.Once
	mu     sync.Mutex
	err    error
	idle   bool
}

func newOpenAIWSAdaptiveRetainedConn(ctx context.Context, conn openAIWSClientConn) (*openAIWSAdaptiveRetainedConn, error) {
	frames, ok := conn.(openaiwsv2.FrameConn)
	if !ok {
		return nil, errors.New("adaptive upstream does not support frame relay")
	}
	ctx, cancel := context.WithCancel(ctx)
	c := &openAIWSAdaptiveRetainedConn{openAIWSClientConn: conn, frames: frames, ctx: ctx, cancel: cancel,
		queue: make(chan openAIWSAdaptiveFrame, 1), done: make(chan struct{})}
	go c.readLoop()
	return c, nil
}

func (c *openAIWSAdaptiveRetainedConn) stop(err error) {
	c.once.Do(func() {
		c.mu.Lock()
		c.err = err
		c.mu.Unlock()
		c.cancel()
		if closer, ok := c.openAIWSClientConn.(openAIWSForceCloser); ok {
			_ = closer.CloseNow()
		} else {
			_ = c.openAIWSClientConn.Close()
		}
	})
}

func (c *openAIWSAdaptiveRetainedConn) readLoop() {
	defer close(c.done)
	defer close(c.queue)
	// Close also unblocks transports that don't promptly observe ctx cancellation.
	watchDone := make(chan struct{})
	defer close(watchDone)
	go func() {
		select {
		case <-c.ctx.Done():
			c.stop(c.ctx.Err())
		case <-watchDone:
		}
	}()
	for {
		kind, payload, err := c.frames.ReadFrame(c.ctx)
		if err != nil {
			c.stop(err)
			return
		}
		c.mu.Lock()
		idle := c.idle
		if !idle {
			event := gjson.GetBytes(payload, "type").String()
			c.idle = isOpenAIWSTerminalEvent(event) || event == "error"
		}
		c.mu.Unlock()
		if idle {
			// Unsolicited data cannot become the next turn's response. Closing
			// also prevents an idle producer from filling an unbounded buffer.
			c.stop(errors.New("unsolicited upstream data between adaptive turns"))
			return
		}
		select {
		case c.queue <- openAIWSAdaptiveFrame{kind: kind, payload: payload}:
		case <-c.ctx.Done():
			return
		}
	}
}

func (c *openAIWSAdaptiveRetainedConn) reuseError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	return c.ctx.Err()
}

func (c *openAIWSAdaptiveRetainedConn) ReadFrame(ctx context.Context) (coderws.MessageType, []byte, error) {
	// Deliver already observed frames (especially terminal usage) before EOF.
	select {
	case frame, ok := <-c.queue:
		if !ok {
			return 0, nil, c.reuseError()
		}
		return frame.kind, frame.payload, nil
	default:
	}
	select {
	case frame, ok := <-c.queue:
		if !ok {
			return 0, nil, c.reuseError()
		}
		return frame.kind, frame.payload, nil
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	}
}

func (c *openAIWSAdaptiveRetainedConn) ReadMessage(ctx context.Context) ([]byte, error) {
	_, payload, err := c.ReadFrame(ctx)
	return payload, err
}

func (c *openAIWSAdaptiveRetainedConn) WriteFrame(ctx context.Context, kind coderws.MessageType, payload []byte) error {
	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		return err
	}
	c.idle = false
	c.mu.Unlock()
	return c.frames.WriteFrame(ctx, kind, payload)
}

func (c *openAIWSAdaptiveRetainedConn) WriteJSON(ctx context.Context, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return c.WriteFrame(ctx, coderws.MessageText, payload)
}

func (c *openAIWSAdaptiveRetainedConn) Close() error {
	c.stop(errOpenAIWSConnClosed)
	<-c.done
	return nil
}
