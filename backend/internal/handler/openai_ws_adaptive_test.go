package handler

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type adaptiveHarnessHTTPUpstream struct{}

func (*adaptiveHarnessHTTPUpstream) Do(r *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return http.DefaultClient.Do(r)
}
func (h *adaptiveHarnessHTTPUpstream) DoWithTLS(r *http.Request, p string, id int64, n int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return h.Do(r, p, id, n)
}

func TestAdaptiveHandlerAcceptsFirstAndSubsequentMessagesAbove64MiB(t *testing.T) {
	if testing.Short() {
		t.Skip("large websocket ingress integration")
	}
	lengths := make(chan int64, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, err := io.Copy(io.Discard, r.Body)
		if err != nil {
			return
		}
		lengths <- n
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_large\",\"model\":\"gpt-5.1\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
	}))
	defer upstream.Close()
	h := newOpenAIWSPassthroughHandlerHarness(t, upstream.URL, func(cfg *config.Config, a *service.Account) {
		a.Extra["openai_apikey_responses_websockets_v2_mode"] = "adaptive"
		cfg.Gateway.OpenAIWS.ClientReadLimitBytes = 128 * 1024 * 1024
		cfg.Gateway.OpenAIWS.HTTPBridgeThresholdBytes = 15 * 1024 * 1024
		cfg.Gateway.OpenAIWS.IngressInterTurnIdleTimeoutSeconds = 30
		cfg.Gateway.OpenAIWS.ClientFirstMessageTimeoutSeconds = 30
	})
	for turn := 0; turn < 2; turn++ {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		// Writer deliberately fragments one message into many network frames.
		writer, err := h.clientConn.Writer(ctx, coderws.MessageText)
		require.NoError(t, err)
		_, err = io.WriteString(writer, `{"type":"response.create","model":"gpt-5.1","input":"`)
		require.NoError(t, err)
		chunk := strings.Repeat("x", 1024*1024)
		for n := 0; n < 65; n++ {
			_, err = io.WriteString(writer, chunk)
			require.NoError(t, err)
		}
		_, err = io.WriteString(writer, `"}`)
		require.NoError(t, err)
		require.NoError(t, writer.Close())
		for {
			_, event, err := h.clientConn.Read(ctx)
			require.NoError(t, err)
			if gjson.GetBytes(event, "type").String() == "response.completed" {
				break
			}
		}
		select {
		case n := <-lengths:
			require.Greater(t, n, int64(64*1024*1024))
		case <-ctx.Done():
			t.Fatal("HTTP request not received")
		}
		cancel()
	}
}

func TestAdaptiveChangePreservesLegacyReadLimit(t *testing.T) {
	calls := make(chan struct{}, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls <- struct{}{}; w.WriteHeader(500) }))
	defer up.Close()
	h := newOpenAIWSPassthroughHandlerHarness(t, up.URL, func(cfg *config.Config, a *service.Account) { cfg.Gateway.OpenAIWS.ClientReadLimitBytes = 1024 })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = h.clientConn.Write(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-5.1","input":"`+strings.Repeat("x", 2048)+`"}`))
	_, _, err := h.clientConn.Read(ctx)
	require.Error(t, err)
	select {
	case <-h.handlerDone:
	case <-ctx.Done():
		t.Fatal("handler did not exit")
	}
	require.Empty(t, calls)
}

func TestAdaptiveHandlerDoesNotStripUnknownPreviousResponse(t *testing.T) {
	calls := make(chan struct{}, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls <- struct{}{}; w.WriteHeader(500) }))
	defer up.Close()
	h := newOpenAIWSPassthroughHandlerHarness(t, up.URL, func(cfg *config.Config, a *service.Account) {
		a.Extra["openai_apikey_responses_websockets_v2_mode"] = "adaptive"
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	require.NoError(t, h.clientConn.Write(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-5.1","previous_response_id":"resp_unknown","input":"delta"}`)))
	_, _, err := h.clientConn.Read(ctx)
	require.Error(t, err)
	require.Empty(t, calls)
}
