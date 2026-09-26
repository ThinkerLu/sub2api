package handler

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestWSModelMigrationHandlerFreezesTargetAndSuppressesDuplicate(t *testing.T) {
	runWSModelMigrationHandler(t, false)
}

func TestWSModelMigrationHandlerAcrossNativeHTTPProvider(t *testing.T) {
	runWSModelMigrationHandler(t, true)
}

func TestWSModelMigrationHandlerFullFrameSwitch(t *testing.T) {
	runWSModelMigrationHandler(t, false, true)
}

func TestWSModelMigrationHandlerFullFrameNativeSwitch(t *testing.T) {
	runWSModelMigrationHandler(t, true, true)
}

func runWSModelMigrationHandler(t *testing.T, nativeProvider bool, fullFrameSwitch ...bool) {
	var mu sync.Mutex
	var models []string
	var bodies [][]byte
	makeEvent := func(body []byte) string {
		model := gjson.GetBytes(body, "model").String()
		mu.Lock()
		models = append(models, model)
		bodies = append(bodies, body)
		turn := len(models)
		mu.Unlock()
		return fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_%d","model":%q,"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`, turn, model)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprintf(w, "data: %s\n\n", makeEvent(body))
			return
		}
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		for {
			_, body, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			event := makeEvent(body)
			if err := conn.Write(r.Context(), coderws.MessageText, []byte(event)); err != nil {
				return
			}
		}
	}))
	defer upstream.Close()
	h := newOpenAIWSPassthroughHandlerHarness(t, upstream.URL, func(cfg *config.Config, a *service.Account) {
		a.Extra["openai_apikey_responses_websockets_v2_mode"] = "adaptive"
		a.Credentials["model_mapping"] = map[string]any{"gpt-5.1": "gpt-5.1"}
	})
	old := h.accountRepo.account
	target := old
	target.ID++
	target.Credentials = map[string]any{"api_key": "sk-target-test", "base_url": upstream.URL, "model_mapping": map[string]any{"deepseek-v4-pro": "deepseek-v4-pro"}}
	if nativeProvider {
		target.Platform = service.PlatformDeepseek
		target.Credentials["api_protocol"] = "responses"
		target.Extra = map[string]any{"responses_ws_mode": "http_bridge"}
	}
	h.accountRepo.accounts = []service.Account{old, target}
	_ = h.clientConn.CloseNow()
	select {
	case <-h.handlerDone:
	case <-time.After(3 * time.Second):
		t.Fatal("initial unused connection did not close")
	}
	if nativeProvider {
		h.apiKey.Group = &service.Group{ID: *h.apiKey.GroupID, Platform: service.PlatformComposite, RateMultiplier: 1}
	}
	dial := func() *coderws.Conn {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		conn, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(h.serverURL, "http")+"/openai/v1/responses", &coderws.DialOptions{HTTPHeader: http.Header{"User-Agent": []string{"codex_cli_rs/0.155.0-alpha.16.4"}, "Thread-Id": []string{"migration-handler-test"}}})
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.CloseNow() })
		return conn
	}
	write := func(conn *coderws.Conn, body string) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, conn.Write(ctx, coderws.MessageText, []byte(body)))
	}
	read := func(conn *coderws.Conn) []byte {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, body, err := conn.Read(ctx)
		require.NoError(t, err)
		return body
	}
	first := dial()
	full := `{"type":"response.create","model":"deepseek-v4-pro","input":[{"role":"user","content":"hello"},{"type":"reasoning","encrypted_content":"gAAAAA-old"},{"role":"assistant","content":"answer"},{"role":"user","content":"next"}]}`
	write(first, `{"type":"response.create","model":"gpt-5.1","input":[{"role":"user","content":"hello"}]}`)
	require.Equal(t, "resp_1", gjson.GetBytes(read(first), "response.id").String())
	if len(fullFrameSwitch) > 0 && fullFrameSwitch[0] {
		write(first, full)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, _, err := first.Read(ctx)
		cancel()
		require.Equal(t, coderws.StatusNormalClosure, coderws.CloseStatus(err))
	} else {
		write(first, `{"type":"response.create","model":"deepseek-v4-pro","previous_response_id":"resp_1","stream_id":"stream-2","input":[{"role":"user","content":"next"}]}`)
		errorEvent := read(first)
		require.Equal(t, "previous_response_not_found", gjson.GetBytes(errorEvent, "error.code").String())
		require.Equal(t, "stream-2", gjson.GetBytes(errorEvent, "stream_id").String())
	}
	mu.Lock()
	require.Len(t, models, 1)
	mu.Unlock()
	second := dial()
	write(second, full)
	require.Equal(t, "resp_2", gjson.GetBytes(read(second), "response.id").String())
	mu.Lock()
	require.Equal(t, []string{"gpt-5.1", "deepseek-v4-pro"}, models)
	require.NotContains(t, string(bodies[1]), "gAAAAA-old")
	require.Empty(t, gjson.GetBytes(bodies[1], "previous_response_id").String())
	mu.Unlock()
	duplicate := dial()
	write(duplicate, full)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, _, err := duplicate.Read(ctx)
	cancel()
	require.Error(t, err)
	mu.Lock()
	require.Len(t, models, 2)
	mu.Unlock()
	backBody := `{"type":"response.create","model":"gpt-5.1","input":[{"role":"user","content":"hello"},{"role":"assistant","content":"answer"},{"role":"user","content":"next"},{"role":"assistant","content":"answer"},{"role":"user","content":"back"}]}`
	if len(fullFrameSwitch) > 0 && fullFrameSwitch[0] {
		write(second, backBody)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, _, err := second.Read(ctx)
		cancel()
		require.Equal(t, coderws.StatusNormalClosure, coderws.CloseStatus(err))
	} else {
		write(second, `{"type":"response.create","model":"gpt-5.1","previous_response_id":"resp_2","input":[{"role":"user","content":"back"}]}`)
		require.Equal(t, "previous_response_not_found", gjson.GetBytes(read(second), "error.code").String())
	}
	back := dial()
	write(back, backBody)
	require.Equal(t, "resp_3", gjson.GetBytes(read(back), "response.id").String())
	mu.Lock()
	require.Equal(t, []string{"gpt-5.1", "deepseek-v4-pro", "gpt-5.1"}, models)
	mu.Unlock()
	require.Eventually(t, func() bool { return len(h.usageRepo.created) == 3 }, 3*time.Second, time.Millisecond)
	for range 3 {
		usage := <-h.usageRepo.created
		accountID := old.ID
		upstream := service.UsageUpstreamTransportWebSocket
		if usage.RequestID == "resp_2" {
			accountID = target.ID
			if nativeProvider {
				upstream = service.UsageUpstreamTransportHTTP
			}
		}
		require.Equal(t, accountID, usage.AccountID)
		require.Equal(t, service.UsageIngressTransportWebSocket, usage.IngressTransport)
		require.Equal(t, upstream, usage.UpstreamTransport)
	}
}
