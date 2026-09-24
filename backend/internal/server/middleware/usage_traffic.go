package middleware

import (
	"bufio"
	"io"
	"net"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// UsageTrafficMiddleware records application-layer request and response body
// bytes for gateway requests. WebSocket frame traffic is added separately by
// the WS forwarding paths because it starts after the HTTP connection is hijacked.
func UsageTrafficMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c == nil || c.Request == nil {
			return
		}

		counter := service.NewUsageTrafficCounter()
		c.Request = c.Request.WithContext(service.WithUsageTrafficCounter(c.Request.Context(), counter))
		c.Request.Body = service.WrapUsageTrafficReadCloser(c.Request.Body, counter.AddIngressBodyBytes)
		c.Writer = &usageTrafficResponseWriter{ResponseWriter: c.Writer, counter: counter}
		c.Next()
	}
}

type usageTrafficResponseWriter struct {
	gin.ResponseWriter
	counter *service.UsageTrafficCounter
}

var _ gin.ResponseWriter = (*usageTrafficResponseWriter)(nil)
var _ io.ReaderFrom = (*usageTrafficResponseWriter)(nil)

func (w *usageTrafficResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *usageTrafficResponseWriter) Write(data []byte) (int, error) {
	n, err := w.ResponseWriter.Write(data)
	w.counter.AddEgressBodyBytes(n)
	return n, err
}

func (w *usageTrafficResponseWriter) WriteString(value string) (int, error) {
	n, err := w.ResponseWriter.WriteString(value)
	w.counter.AddEgressBodyBytes(n)
	return n, err
}

func (w *usageTrafficResponseWriter) ReadFrom(r io.Reader) (int64, error) {
	if readerFrom, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		n, err := readerFrom.ReadFrom(r)
		if n > 0 {
			w.counter.AddEgressBodyBytes(int(n))
		}
		return n, err
	}
	n, err := io.Copy(w.ResponseWriter, r)
	if n > 0 {
		w.counter.AddEgressBodyBytes(int(n))
	}
	return n, err
}

func (w *usageTrafficResponseWriter) Flush() {
	w.ResponseWriter.Flush()
}

func (w *usageTrafficResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.ResponseWriter.Hijack()
}

func (w *usageTrafficResponseWriter) CloseNotify() <-chan bool {
	return w.ResponseWriter.CloseNotify()
}

func (w *usageTrafficResponseWriter) Pusher() http.Pusher {
	return w.ResponseWriter.Pusher()
}
