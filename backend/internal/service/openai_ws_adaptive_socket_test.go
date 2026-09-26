package service

import (
	"encoding/json"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWSAdaptiveSocketLimitAndIdempotentRelease(t *testing.T) {
	s := &OpenAIGatewayService{}
	release, err := s.acquireOpenAIWSAdaptiveSocket(1, 1)
	require.NoError(t, err)
	_, err = s.acquireOpenAIWSAdaptiveSocket(1, 1)
	require.ErrorIs(t, err, errOpenAIWSAdaptiveSocketLimit)
	other, err := s.acquireOpenAIWSAdaptiveSocket(2, 1)
	require.NoError(t, err)
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() { defer wg.Done(); release() }()
	}
	wg.Wait()
	other()
	require.Empty(t, s.openaiWSAdaptiveSockets)
}

func TestWSRetainedInputDoesNotAliasTheSourceFrame(t *testing.T) {
	frame := []byte(`padding {"role":"user","content":"small"} trailing`)
	item := json.RawMessage(frame[8 : len(frame)-9])
	owned := ownedOpenAIWSReplayItems([]json.RawMessage{item})
	require.Equal(t, string(item), string(owned[0]))
	require.Equal(t, len(owned[0]), cap(owned[0]))
	clear(frame)
	require.Contains(t, string(owned[0]), "small")
}

func TestDualLegLargeFrameWithoutBudget(t *testing.T) {
	if os.Getenv("SUB2API_WS_STRESS") != "1" {
		t.Skip("opt-in 16 MiB frame validation without memory admission")
	}
	s, account, ws, httpUp := adaptiveFixture()
	server, _ := startAdaptiveServer(t, s, account, nil)
	client := dialAdaptive(t, server)
	payload := `{"type":"response.create","model":"deepseek-v4-pro","input":[{"role":"user","content":"` + strings.Repeat("x", 16<<20) + `"}]}`
	writeAdaptive(t, client, payload)
	forwarded := requirePassthroughUpstreamWrite(t, ws, 10*time.Second)
	require.Greater(t, len(forwarded), 15<<20)
	require.Empty(t, httpUp.snapshot())
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	if status, err := os.ReadFile("/proc/self/status"); err == nil {
		for _, line := range strings.Split(string(status), "\n") {
			if strings.HasPrefix(line, "VmHWM:") {
				t.Log(line)
			}
		}
	}
	t.Logf("input_bytes=%d heap_alloc_bytes=%d", len(payload), stats.HeapAlloc)
	ws.Send(`{"type":"response.completed","response":{"id":"resp_large","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`)
	readAdaptiveCompleted(t, client)
	_ = client.CloseNow()
	require.Eventually(t, func() bool {
		s.openaiWSAdaptiveSocketMu.Lock()
		defer s.openaiWSAdaptiveSocketMu.Unlock()
		return len(s.openaiWSAdaptiveSockets) == 0
	}, 3*time.Second, time.Millisecond)
}
