package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type dualLegFailFirstHTTP struct {
	adaptiveHTTPUpstream
	attempts atomic.Int32
}

func (u *dualLegFailFirstHTTP) Do(r *http.Request, p string, a int64, n int) (*http.Response, error) {
	if u.attempts.Add(1) == 1 {
		return nil, io.EOF
	}
	return u.adaptiveHTTPUpstream.Do(r, p, a, n)
}

type dualLegFailedDialer struct{}

func (*dualLegFailedDialer) Dial(context.Context, string, http.Header, string) (openAIWSClientConn, int, http.Header, error) {
	return nil, http.StatusBadGateway, nil, io.EOF
}

func TestDualLegAlternatesUpstreamAtTurnBoundaries(t *testing.T) {
	s, account, firstWS, httpUp := adaptiveFixture()
	s.cfg.Gateway.OpenAIWS.DualLegEnabled = true
	s.cfg.Gateway.OpenAIWS.GPTThresholdRoutingEnabled = true
	secondWS := newStagedPassthroughConn()
	dialer := &adaptiveWarmupDialer{conns: make(chan *stagedPassthroughConn, 2)}
	dialer.conns <- firstWS
	dialer.conns <- secondWS
	s.openaiWSPassthroughDialer = dialer
	var mu sync.Mutex
	var turns []int
	var httpTurns []bool
	server, errs := startAdaptiveServer(t, s, account, &OpenAIWSIngressHooks{
		AfterTurn: func(turn int, result *OpenAIForwardResult, err error) {
			if err == nil && result != nil {
				mu.Lock()
				defer mu.Unlock()
				turns = append(turns, turn)
				httpTurns = append(httpTurns, result.OpenAIWSHTTPBridge)
			}
		},
	})
	client := dialAdaptive(t, server)
	writeAdaptive(t, client, `{"type":"response.create","model":"gpt-5.1","input":"`+strings.Repeat("x", 1200)+`"}`)
	readAdaptiveCompleted(t, client)
	writeAdaptive(t, client, `{"type":"response.create","model":"gpt-5.1","input":"new compact window"}`)
	request := requirePassthroughUpstreamWrite(t, firstWS, time.Second)
	require.Empty(t, gjson.GetBytes(request, "previous_response_id").String())
	firstWS.Send(`{"type":"response.completed","response":{"id":"resp_ws_1","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"portable answer"}]}],"usage":{"input_tokens":1,"output_tokens":1}}}`)
	readAdaptiveCompleted(t, client)
	writeAdaptive(t, client, `{"type":"response.create","model":"gpt-5.1","previous_response_id":"resp_ws_1","input":[{"role":"user","content":"`+strings.Repeat("y", 1200)+`"}]}`)
	readAdaptiveCompleted(t, client)
	requests := httpUp.snapshot()
	require.Len(t, requests, 2)
	require.Empty(t, gjson.GetBytes(requests[1], "previous_response_id").String())
	require.Contains(t, string(requests[1]), "portable answer")
	select {
	case <-firstWS.closed:
	default:
		t.Fatal("HTTP turn retained the old WS socket")
	}
	writeAdaptive(t, client, `{"type":"response.create","model":"gpt-5.1","input":"another compact window"}`)
	request = requirePassthroughUpstreamWrite(t, secondWS, time.Second)
	require.Empty(t, gjson.GetBytes(request, "previous_response_id").String())
	secondWS.Send(`{"type":"response.completed","response":{"id":"resp_ws_2","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`)
	readAdaptiveCompleted(t, client)
	require.Eventually(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(turns) == 4 }, time.Second, time.Millisecond)
	mu.Lock()
	require.Equal(t, []int{1, 2, 3, 4}, turns)
	require.Equal(t, []bool{true, false, true, false}, httpTurns)
	mu.Unlock()
	_ = client.CloseNow()
	select {
	case <-errs:
	case <-time.After(3 * time.Second):
		t.Fatal("dual-leg session did not exit")
	}
}

func TestDualLegHTTPUnknownSubmissionCannotFailover(t *testing.T) {
	s, account, _, _ := adaptiveFixture()
	s.cfg.Gateway.OpenAIWS.DualLegEnabled = true
	s.cfg.Gateway.OpenAIWS.HTTPBridgeThresholdBytes = 1
	upstream := &dualLegFailFirstHTTP{}
	s.httpUpstream = upstream
	server, errs := startAdaptiveServer(t, s, account, nil)
	client := dialAdaptive(t, server)
	writeAdaptive(t, client, `{"type":"response.create","model":"gpt-5.1","input":"hello"}`)
	event, err := readPassthroughLifecycleFrame(t, client, time.Second)
	require.NoError(t, err)
	require.Equal(t, "upstream_execution_unknown", gjson.GetBytes(event, "error.code").String())
	require.EqualValues(t, 1, upstream.attempts.Load())
	writeAdaptive(t, client, `{"type":"response.create","model":"gpt-5.1","input":"new independent turn"}`)
	readAdaptiveCompleted(t, client)
	require.EqualValues(t, 2, upstream.attempts.Load())
	_ = client.CloseNow()
	select {
	case <-errs:
	case <-time.After(3 * time.Second):
		t.Fatal("session did not exit")
	}
}

func TestDualLegHandshakeFailureFallsBackBeforeSubmission(t *testing.T) {
	s, account, _, httpUp := adaptiveFixture()
	s.cfg.Gateway.OpenAIWS.DualLegEnabled = true
	s.openaiWSPassthroughDialer = &dualLegFailedDialer{}
	server, _ := startAdaptiveServer(t, s, account, nil)
	client := dialAdaptive(t, server)
	writeAdaptive(t, client, `{"type":"response.create","model":"gpt-5.1","input":"hello"}`)
	readAdaptiveCompleted(t, client)
	require.Len(t, httpUp.snapshot(), 1)
}

func TestDualLegSameAccountProtocolSwitchRebuildsPortableHistory(t *testing.T) {
	s, account, first, httpUp := adaptiveFixture()
	s.cfg.Gateway.OpenAIWS.DualLegEnabled = true
	second := newStagedPassthroughConn()
	dialer := &adaptiveWarmupDialer{conns: make(chan *stagedPassthroughConn, 2)}
	dialer.conns <- first
	dialer.conns <- second
	s.openaiWSPassthroughDialer = dialer
	server, _ := startAdaptiveServer(t, s, account, nil)
	client := dialAdaptive(t, server)
	writeAdaptive(t, client, `{"type":"response.create","model":"gpt-5.1","input":[{"role":"user","content":"hello"}]}`)
	requirePassthroughUpstreamWrite(t, first, time.Second)
	first.Send(`{"type":"response.completed","response":{"id":"resp_gpt","output":[{"type":"reasoning","encrypted_content":"gAAAAA-private"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}],"usage":{"input_tokens":1,"output_tokens":1}}}`)
	readAdaptiveCompleted(t, client)
	writeAdaptive(t, client, `{"type":"response.create","model":"deepseek-v4-pro","previous_response_id":"resp_gpt","input":[{"role":"user","content":"next"}]}`)
	body := requirePassthroughUpstreamWrite(t, second, time.Second)
	require.Empty(t, gjson.GetBytes(body, "previous_response_id").String())
	require.NotContains(t, string(body), "gAAAAA-private")
	require.Contains(t, string(body), "hello")
	require.Contains(t, string(body), "answer")
	require.Equal(t, "deepseek-v4-pro", gjson.GetBytes(body, "model").String())
	second.Send(`{"type":"response.completed","response":{"id":"resp_deepseek","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`)
	readAdaptiveCompleted(t, client)
	require.Empty(t, httpUp.snapshot())
}

func TestDualLegCommitStateNeverBecomesRetrySafeAgain(t *testing.T) {
	c := &openAIWSTurnCommit{}
	require.True(t, c.notSent())
	for _, state := range []openAIWSTurnCommitState{openAIWSTurnSentUnknown, openAIWSTurnExecuting, openAIWSTurnTerminal, openAIWSTurnNotSent} {
		c.advance(state)
		require.False(t, c.notSent())
	}
	require.Equal(t, int32(openAIWSTurnTerminal), c.state.Load())
}
