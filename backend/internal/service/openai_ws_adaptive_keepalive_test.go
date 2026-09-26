package service

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestAdaptiveRetainedUpstreamAnswersIdlePing(t *testing.T) {
	ping := make(chan struct{})
	pong := make(chan error, 1)
	upstreamDone := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow(); upstreamDone <- struct{}{} }()
		go func() {
			select {
			case <-ping:
			case <-r.Context().Done():
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), time.Second)
			defer cancel()
			pong <- conn.Ping(ctx)
		}()
		for n := 1; ; n++ {
			_, _, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			body := fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_idle_%d","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`, n)
			if conn.Write(r.Context(), coderws.MessageText, []byte(body)) != nil {
				return
			}
		}
	}))
	defer upstream.Close()
	s, account, _, _ := adaptiveFixture()
	s.cfg.Gateway.OpenAIWS.IngressInterTurnIdleTimeoutSeconds = 10
	dialer := &adaptiveLocalDialer{url: "ws" + strings.TrimPrefix(upstream.URL, "http")}
	s.openaiWSPassthroughDialer = dialer
	completed := make(chan struct{}, 2)
	server, errs := startAdaptiveServer(t, s, account, &OpenAIWSIngressHooks{AfterTurn: func(_ int, _ *OpenAIForwardResult, err error) {
		if err == nil {
			completed <- struct{}{}
		}
	}})
	client := dialAdaptive(t, server)
	writeAdaptive(t, client, `{"type":"response.create","model":"gpt-5.1","input":"first"}`)
	readAdaptiveCompleted(t, client)
	select {
	case <-completed:
	case <-time.After(3 * time.Second):
		t.Fatal("single-turn relay did not finish")
	}
	close(ping)
	select {
	case err := <-pong:
		require.NoError(t, err, "upstream ping must be answered between turns")
	case <-time.After(3 * time.Second):
		t.Fatal("idle pong missing")
	}
	writeAdaptive(t, client, `{"type":"response.create","model":"gpt-5.1","previous_response_id":"resp_idle_1","input":"second"}`)
	readAdaptiveCompleted(t, client)
	require.EqualValues(t, 1, dialer.calls.Load())
	_ = client.CloseNow()
	select {
	case <-errs:
	case <-time.After(3 * time.Second):
		t.Fatal("session leaked")
	}
	select {
	case <-upstreamDone:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream reader leaked")
	}
}

func TestAdaptiveIdleCloseRebuildsFullContextBeforeSubmission(t *testing.T) {
	for _, reason := range []string{"peer_close", "unsolicited_data"} {
		t.Run(reason, func(t *testing.T) { testAdaptiveIdleInvalidation(t, reason) })
	}
}

func testAdaptiveIdleInvalidation(t *testing.T, reason string) {
	closed := make(chan struct{})
	closeNow := make(chan struct{})
	requests := make(chan []byte, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		_, body, err := conn.Read(r.Context())
		if err != nil {
			return
		}
		requests <- body
		first := gjson.GetBytes(body, "input").String() == "first"
		id := "resp_rebuilt"
		if first {
			id = "resp_idle_closed"
		}
		_ = conn.Write(r.Context(), coderws.MessageText, []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":%q,"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}],"usage":{"input_tokens":1,"output_tokens":1}}}`, id)))
		if first {
			<-closeNow
			if reason == "unsolicited_data" {
				_ = conn.Write(r.Context(), coderws.MessageText, []byte(`{"type":"response.output_text.delta","delta":"stale"}`))
				_, _, _ = conn.Read(r.Context())
			}
			_ = conn.Close(coderws.StatusGoingAway, "idle close")
			close(closed)
		} else {
			_, _, _ = conn.Read(r.Context())
		}
	}))
	defer upstream.Close()
	s, account, _, _ := adaptiveFixture()
	s.cfg.Gateway.OpenAIWS.IngressInterTurnIdleTimeoutSeconds = 10
	dialer := &adaptiveLocalDialer{url: "ws" + strings.TrimPrefix(upstream.URL, "http")}
	s.openaiWSPassthroughDialer = dialer
	server, _ := startAdaptiveServer(t, s, account, nil)
	client := dialAdaptive(t, server)
	writeAdaptive(t, client, `{"type":"response.create","model":"gpt-5.1","input":"first"}`)
	readAdaptiveCompleted(t, client)
	<-requests
	close(closeNow)
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("idle close not observed")
	}
	writeAdaptive(t, client, `{"type":"response.create","model":"gpt-5.1","previous_response_id":"resp_idle_closed","input":"second"}`)
	readAdaptiveCompleted(t, client)
	select {
	case body := <-requests:
		require.Empty(t, gjson.GetBytes(body, "previous_response_id").String())
		require.Contains(t, string(body), "first")
		require.Contains(t, string(body), "answer")
		require.Contains(t, string(body), "second")
	case <-time.After(3 * time.Second):
		t.Fatal("full replay missing")
	}
	require.EqualValues(t, 2, dialer.calls.Load())
}

func TestAdaptiveRetainedReaderCancellationDoesNotStopTransport(t *testing.T) {
	send := make(chan struct{})
	upstreamDone := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow(); close(upstreamDone) }()
		select {
		case <-send:
		case <-r.Context().Done():
			return
		}
		_ = conn.Write(r.Context(), coderws.MessageBinary, []byte("binary frame"))
		_, _, _ = conn.Read(r.Context())
	}))
	defer upstream.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	raw, _, _, err := newDefaultOpenAIWSClientDialer().Dial(ctx, "ws"+strings.TrimPrefix(upstream.URL, "http"), nil, "")
	require.NoError(t, err)
	conn, err := newOpenAIWSAdaptiveRetainedConn(ctx, raw)
	require.NoError(t, err)
	defer conn.Close()
	turnCtx, cancelTurn := context.WithCancel(ctx)
	cancelTurn()
	_, _, err = conn.ReadFrame(turnCtx)
	require.ErrorIs(t, err, context.Canceled)
	require.NoError(t, conn.reuseError())
	close(send)
	kind, payload, err := conn.ReadFrame(ctx)
	require.NoError(t, err)
	require.Equal(t, coderws.MessageBinary, kind)
	require.Equal(t, "binary frame", string(payload))
	require.NoError(t, conn.Close())
	select {
	case <-upstreamDone:
	case <-ctx.Done():
		t.Fatal("upstream connection leaked")
	}
}

func TestAdaptiveRetainedReaderDeliversTerminalBeforePeerEOF(t *testing.T) {
	terminal := `{"type":"response.completed","response":{"id":"resp_last","usage":{"input_tokens":9,"output_tokens":3}}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			return
		}
		_ = conn.Write(r.Context(), coderws.MessageText, []byte(terminal))
		_ = conn.CloseNow()
	}))
	defer upstream.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	raw, _, _, err := newDefaultOpenAIWSClientDialer().Dial(ctx, "ws"+strings.TrimPrefix(upstream.URL, "http"), nil, "")
	require.NoError(t, err)
	conn, err := newOpenAIWSAdaptiveRetainedConn(ctx, raw)
	require.NoError(t, err)
	defer conn.Close()
	select {
	case <-conn.done:
	case <-ctx.Done():
		t.Fatal("peer close not observed")
	}
	require.Error(t, conn.reuseError())
	_, payload, err := conn.ReadFrame(ctx)
	require.NoError(t, err)
	require.Equal(t, terminal, string(payload))
	_, _, err = conn.ReadFrame(ctx)
	require.Error(t, err)
}
