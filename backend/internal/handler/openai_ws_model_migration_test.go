package handler

import (
	"context"
	"fmt"
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
	var mu sync.Mutex
	var models []string
	var bodies [][]byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			model := gjson.GetBytes(body, "model").String()
			mu.Lock()
			models = append(models, model)
			bodies = append(bodies, body)
			turn := len(models)
			mu.Unlock()
			event := fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_%d","model":%q,"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}],"usage":{"input_tokens":1,"output_tokens":1}}}`, turn, model)
			if err := conn.Write(r.Context(), coderws.MessageText, []byte(event)); err != nil {
				return
			}
		}
	}))
	defer upstream.Close()
	h := newOpenAIWSPassthroughHandlerHarness(t, upstream.URL, func(cfg *config.Config, a *service.Account) {
		a.Extra["openai_apikey_responses_websockets_v2_mode"] = "adaptive"
		a.Credentials["model_mapping"] = map[string]any{"gpt-5.1": "gpt-5.1"}
		cfg.Gateway.OpenAIWS.DualLegEnabled = true
		cfg.Gateway.OpenAIWS.DualLegMemoryBudgetBytes = 1 << 30
		cfg.Gateway.OpenAIWS.DualLegAccountMemoryBudgetBytes = 512 << 20
		cfg.Gateway.OpenAIWS.ModelSwitchFullRetryEnabled = true
		cfg.Gateway.OpenAIWS.ModelSwitchAllowedGroupIDs = []int64{4301}
		cfg.Gateway.OpenAIWS.ModelSwitchAllowedClientVersions = []string{"0.155.0-alpha.16.4"}
	})
	old := h.accountRepo.account
	target := old
	target.ID++
	target.Credentials = map[string]any{"api_key": "sk-target-test", "base_url": upstream.URL, "model_mapping": map[string]any{"deepseek-v4-pro": "deepseek-v4-pro"}}
	h.accountRepo.accounts = []service.Account{old, target}
	_ = h.clientConn.CloseNow()
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
	write(first, `{"type":"response.create","model":"gpt-5.1","input":[{"role":"user","content":"hello"}]}`)
	require.Equal(t, "resp_1", gjson.GetBytes(read(first), "response.id").String())
	write(first, `{"type":"response.create","model":"deepseek-v4-pro","previous_response_id":"resp_1","stream_id":"stream-2","input":[{"role":"user","content":"next"}]}`)
	errorEvent := read(first)
	require.Equal(t, "previous_response_not_found", gjson.GetBytes(errorEvent, "error.code").String())
	require.Equal(t, "stream-2", gjson.GetBytes(errorEvent, "stream_id").String())
	mu.Lock()
	require.Len(t, models, 1)
	mu.Unlock()
	full := `{"type":"response.create","model":"deepseek-v4-pro","input":[{"role":"user","content":"hello"},{"type":"reasoning","encrypted_content":"gAAAAA-old"},{"role":"assistant","content":"answer"},{"role":"user","content":"next"}]}`
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
	write(second, `{"type":"response.create","model":"gpt-5.1","previous_response_id":"resp_2","input":[{"role":"user","content":"back"}]}`)
	require.Equal(t, "previous_response_not_found", gjson.GetBytes(read(second), "error.code").String())
	back := dial()
	write(back, `{"type":"response.create","model":"gpt-5.1","input":[{"role":"user","content":"hello"},{"role":"assistant","content":"answer"},{"role":"user","content":"next"},{"role":"assistant","content":"answer"},{"role":"user","content":"back"}]}`)
	require.Equal(t, "resp_3", gjson.GetBytes(read(back), "response.id").String())
	mu.Lock()
	require.Equal(t, []string{"gpt-5.1", "deepseek-v4-pro", "gpt-5.1"}, models)
	mu.Unlock()
	require.Eventually(t, func() bool { return len(h.usageRepo.created) == 3 }, 3*time.Second, time.Millisecond)
	for _, accountID := range []int64{old.ID, target.ID, old.ID} {
		usage := <-h.usageRepo.created
		require.Equal(t, accountID, usage.AccountID)
	}
}
