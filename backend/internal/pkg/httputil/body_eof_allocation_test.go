package httputil

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"testing"
)

func TestReadBodyEOFSlack(t *testing.T) {
	for _, hint := range []int64{-1, 0, 7, 1 << 20, 1 << 40} {
		body := bytes.Repeat([]byte("x"), 1<<20)
		req := &http.Request{Body: io.NopCloser(memoryOptimizationReader{bytes.NewReader(body)}), ContentLength: hint}
		got, err := ReadRequestBodyWithPrealloc(req)
		if err != nil || !bytes.Equal(got, body) {
			t.Fatalf("hint %d: %v", hint, err)
		}
		if hint >= 1<<20 && cap(got) > len(body)+8192 {
			t.Fatalf("EOF caused growth: cap=%d", cap(got))
		}
	}
	req := &http.Request{Body: http.MaxBytesReader(nil, io.NopCloser(bytes.NewReader(make([]byte, 1025))), 1024), ContentLength: 1025}
	_, err := ReadRequestBodyWithPrealloc(req)
	var limit *http.MaxBytesError
	if !errors.As(err, &limit) {
		t.Fatalf("lost body limit error: %v", err)
	}
}
