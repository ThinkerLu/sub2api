package httputil

import (
	"bytes"
	"io"
	"net/http"
	"testing"
)

type memoryOptimizationReader struct{ io.Reader }

var memoryOptimizationBody []byte

func BenchmarkMemoryOptimizationHTTPRead(b *testing.B) {
	for _, size := range []int{1 << 20, 8 << 20, 16 << 20} {
		body := bytes.Repeat([]byte("a"), size)
		b.Run(http.StatusText(size)+fmtSize(size), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(size))
			for b.Loop() {
				req := &http.Request{Body: io.NopCloser(memoryOptimizationReader{bytes.NewReader(body)}), ContentLength: int64(size)}
				var err error
				memoryOptimizationBody, err = ReadRequestBodyWithPrealloc(req)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
func fmtSize(size int) string {
	switch size {
	case 1 << 20:
		return "1MiB"
	case 8 << 20:
		return "8MiB"
	default:
		return "16MiB"
	}
}
