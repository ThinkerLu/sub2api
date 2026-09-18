package service

import (
	"bufio"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	coderws "github.com/coder/websocket"
)

// OpenAIWSReadProgress observes transport reads only during the first message.
// This must sit below coder/websocket's io.ReadFull and decompression buffers.
// Control frames also count as traffic; this is an inactivity limit, not a
// minimum upload rate or a total session deadline.
type OpenAIWSReadProgress struct {
	origin    time.Time
	last      atomic.Int64
	bytes     atomic.Int64
	stoppedAt atomic.Int64
}

func (p *OpenAIWSReadProgress) touch()           { p.last.Store(int64(time.Since(p.origin))) }
func (p *OpenAIWSReadProgress) stop()            { p.stoppedAt.CompareAndSwap(0, int64(time.Since(p.origin))) }
func (p *OpenAIWSReadProgress) BytesRead() int64 { return p.bytes.Load() }
func (p *OpenAIWSReadProgress) IdleDuration() time.Duration {
	last := time.Duration(p.last.Load())
	now := time.Duration(p.stoppedAt.Load())
	if now == 0 {
		now = time.Since(p.origin)
	}
	// A read already in flight may publish just after stop freezes the clock.
	if now < last {
		return 0
	}
	return now - last
}

func (p *OpenAIWSReadProgress) observe(n int) {
	if n > 0 && p.stoppedAt.Load() == 0 {
		p.touch()
		p.bytes.Add(int64(n))
	}
}

// AcceptOpenAIWSClient preserves the normal upgrade, framing and read limits.
// The returned progress observer must be passed to ReadOpenAIWSClientFirstMessage.
func AcceptOpenAIWSClient(w http.ResponseWriter, r *http.Request, opts *coderws.AcceptOptions) (*coderws.Conn, *OpenAIWSReadProgress, error) {
	p := &OpenAIWSReadProgress{origin: time.Now()}
	conn, err := coderws.Accept(&openAIWSProgressWriter{ResponseWriter: w, progress: p}, r, opts)
	return conn, p, err
}

type openAIWSProgressWriter struct {
	http.ResponseWriter
	progress *OpenAIWSReadProgress
}

// Gin delays headers until this method is called. Keep coder/websocket's
// existing flush hook when wrapping Gin's ResponseWriter.
func (w *openAIWSProgressWriter) WriteHeaderNow() {
	if flusher, ok := w.ResponseWriter.(interface{ WriteHeaderNow() }); ok {
		flusher.WriteHeaderNow()
	}
}

func (w *openAIWSProgressWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err != nil {
		return nil, nil, err
	}
	w.progress.observe(rw.Reader.Buffered())
	// coder/websocket preserves the buffered bytes and resets its reader onto
	// this returned connection. Do not reset/discard the HTTP read buffer here.
	return &openAIWSProgressConn{Conn: conn, progress: w.progress}, rw, nil
}

type openAIWSProgressConn struct {
	net.Conn
	progress *OpenAIWSReadProgress
}

func (c *openAIWSProgressConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.progress.observe(n)
	return n, err
}
