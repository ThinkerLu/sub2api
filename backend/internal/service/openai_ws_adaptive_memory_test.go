package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

type adaptiveDiscardHTTP struct{}

func (*adaptiveDiscardHTTP) Do(r *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	if _, err := io.Copy(io.Discard, r.Body); err != nil {
		return nil, err
	}
	_ = r.Body.Close()
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_memory\",\"model\":\"gpt-5.1\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"))}, nil
}
func (h *adaptiveDiscardHTTP) DoWithTLS(r *http.Request, p string, id int64, n int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return h.Do(r, p, id, n)
}
func adaptiveHeapAlloc() uint64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}
func writeAdaptiveSizedPayload(t *testing.T, c *coderws.Conn, n int, image bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	w, err := c.Writer(ctx, coderws.MessageText)
	require.NoError(t, err)
	prefix := `{"type":"response.create","model":"gpt-5.1","input":[{"role":"user","content":"`
	suffix := `"}]}`
	if image {
		prefix = `{"type":"response.create","model":"gpt-5.1","input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,`
		suffix = `"}]}]}`
	}
	_, err = io.WriteString(w, prefix)
	require.NoError(t, err)
	chunk := strings.Repeat("A", 64*1024)
	for n > 0 {
		count := min(n, len(chunk))
		_, err = io.WriteString(w, chunk[:count])
		require.NoError(t, err)
		n -= count
	}
	_, err = io.WriteString(w, suffix)
	require.NoError(t, err)
	require.NoError(t, w.Close())
}
func respondAdaptiveProbeWS(t *testing.T, ws *stagedPassthroughConn) {
	t.Helper()
	_ = requirePassthroughUpstreamWrite(t, ws, 5*time.Second)
	ws.Send(`{"type":"response.created","response":{"id":"resp_memory_ws","model":"gpt-5.1"}}`)
	ws.Send(`{"type":"response.output_text.delta","response_id":"resp_memory_ws","delta":"ok"}`)
	ws.Send(`{"type":"response.completed","response":{"id":"resp_memory_ws","model":"gpt-5.1","usage":{"input_tokens":1,"output_tokens":1}}}`)
}

// Opt-in diagnostic: measures actual ingress, policy/replay and HTTP/WS event
// paths with local upstreams. Heap figures include the local client and server.
func TestAdaptiveMemoryProfile(t *testing.T) {
	if os.Getenv("SUB2API_ADAPTIVE_PROFILE") != "1" {
		t.Skip("set SUB2API_ADAPTIVE_PROFILE=1 to run the memory/TTFT comparison")
	}
	for _, sample := range []struct {
		name  string
		size  int
		image bool
	}{{"small", 64 * 1024, false}, {"history", 20 * 1024 * 1024, false}, {"images", 20 * 1024 * 1024, true}, {"large65", 65 * 1024 * 1024, false}} {
		for _, mode := range []string{OpenAIWSIngressModePassthrough, OpenAIWSIngressModeAdaptive} {
			t.Run(sample.name+"/"+mode, func(t *testing.T) {
				s, a, ws, _ := adaptiveFixture()
				s.httpUpstream = &adaptiveDiscardHTTP{}
				s.cfg.Gateway.OpenAIWS.HTTPBridgeEnabled = true
				s.cfg.Gateway.OpenAIWS.HTTPBridgeThresholdBytes = 15 * 1024 * 1024
				s.cfg.Gateway.OpenAIWS.IngressInterTurnIdleTimeoutSeconds = 45
				a.Extra["openai_apikey_responses_websockets_v2_mode"] = mode
				// For the legacy large65 comparison, raise only the test ingress limit so
				// both paths can be measured. The production legacy default remains 64 MiB.
				if sample.size > 64*1024*1024 {
					s.cfg.Gateway.OpenAIWS.ClientReadLimitBytes = 80 * 1024 * 1024
				}
				var mu sync.Mutex
				var ttft []*int
				hooks := &OpenAIWSIngressHooks{AfterTurn: func(_ int, r *OpenAIForwardResult, err error) {
					if r != nil && err == nil {
						mu.Lock()
						ttft = append(ttft, r.FirstTokenMs)
						mu.Unlock()
					}
				}}
				server, _ := startAdaptiveServer(t, s, a, hooks)
				c := dialAdaptive(t, server)
				baseline := adaptiveHeapAlloc()
				var peak atomic.Uint64
				peak.Store(baseline)
				stop := make(chan struct{})
				done := make(chan struct{})
				go func() {
					defer close(done)
					ticker := time.NewTicker(2 * time.Millisecond)
					defer ticker.Stop()
					for {
						select {
						case <-stop:
							return
						case <-ticker.C:
							var m runtime.MemStats
							runtime.ReadMemStats(&m)
							for old := peak.Load(); m.HeapAlloc > old; old = peak.Load() {
								if peak.CompareAndSwap(old, m.HeapAlloc) {
									break
								}
							}
						}
					}
				}()
				started := time.Now()
				writeAdaptiveSizedPayload(t, c, sample.size, sample.image)
				if sample.size < 15*1024*1024 {
					respondAdaptiveProbeWS(t, ws)
				}
				readAdaptiveCompleted(t, c)
				firstWall := time.Since(started)
				bridgeHeap := adaptiveHeapAlloc()
				writeAdaptive(t, c, `{"type":"response.create","model":"gpt-5.1","input":[{"type":"compaction","encrypted_content":"summary"},{"role":"user","content":"continue"}]}`)
				if mode == OpenAIWSIngressModeAdaptive || sample.size < 15*1024*1024 {
					respondAdaptiveProbeWS(t, ws)
				}
				readAdaptiveCompleted(t, c)
				require.Eventually(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(ttft) >= 2 }, time.Second, time.Millisecond)
				after := adaptiveHeapAlloc()
				close(stop)
				<-done
				mu.Lock()
				firstMS, secondMS := -1, -1
				if ttft[0] != nil {
					firstMS = *ttft[0]
				}
				if ttft[1] != nil {
					secondMS = *ttft[1]
				}
				mu.Unlock()
				t.Logf("PROFILE sample=%s mode=%s input_bytes=%d baseline=%d peak_heap_delta=%d bridge_heap_delta=%d compact_heap_delta=%d first_wall_ms=%d first_ttft_ms=%d compact_ttft_ms=%d", sample.name, mode, sample.size, baseline, int64(peak.Load())-int64(baseline), int64(bridgeHeap)-int64(baseline), int64(after)-int64(baseline), firstWall.Milliseconds(), firstMS, secondMS)
				if mode == OpenAIWSIngressModeAdaptive && sample.size > 15*1024*1024 {
					require.Less(t, int64(after)-int64(baseline), int64(sample.size/2), fmt.Sprintf("pre-compaction history still retained for %s", sample.name))
				}
			})
		}
	}
}
