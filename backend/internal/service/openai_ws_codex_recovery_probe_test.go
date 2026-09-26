package service

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Opt-in client protocol verification. The isolated CLI only receives synthetic
// local responses, has no real credentials, and is never asked to execute tools.
func TestCodexWSRecoveryProbe(t *testing.T) {
	binary := os.Getenv("SUB2API_CODEX_PROBE_BIN")
	if binary == "" {
		t.Skip("set SUB2API_CODEX_PROBE_BIN to the verified Codex CLI")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	var mu sync.Mutex
	var handlers sync.WaitGroup
	connections := 0
	sentError := false
	recovered := false
	incrementalItems := 0
	fullItems := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlers.Add(1)
		defer handlers.Done()
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			http.Error(w, "local probe requires websocket", 400)
			return
		}
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		conn.SetReadLimit(64 * 1024 * 1024)
		mu.Lock()
		connections++
		mu.Unlock()
		for {
			_, body, err := conn.Read(ctx)
			if err != nil {
				t.Logf("probe read ended: %v", err)
				return
			}
			t.Logf("probe frame type=%s generate=%s bytes=%d items=%d previous=%v", gjson.GetBytes(body, "type").String(), gjson.GetBytes(body, "generate").Raw, len(body), len(gjson.GetBytes(body, "input").Array()), gjson.GetBytes(body, "previous_response_id").String() != "")
			if gjson.GetBytes(body, "generate").Type == gjson.False {
				_ = conn.Write(ctx, coderws.MessageText, []byte(`{"type":"response.completed","response":{"id":"resp_probe_warmup","status":"completed","output":[],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`))
				continue
			}
			mu.Lock()
			if !sentError {
				if gjson.GetBytes(body, "previous_response_id").String() == "" {
					mu.Unlock()
					return
				}
				sentError = true
				incrementalItems = len(gjson.GetBytes(body, "input").Array())
				mu.Unlock()
				_ = conn.Write(ctx, coderws.MessageText, []byte(`{"type":"error","status":400,"error":{"type":"invalid_request_error","code":"previous_response_not_found","param":"previous_response_id","message":"Previous response not found."}}`))
				continue
			}
			fullItems = len(gjson.GetBytes(body, "input").Array())
			recovered = gjson.GetBytes(body, "previous_response_id").String() == "" && strings.Contains(string(body), "SYNTHETIC_WS_RECOVERY_MARKER")
			mu.Unlock()
			for _, event := range []string{
				`{"type":"response.created","response":{"id":"resp_probe_final","status":"in_progress","output":[]}}`,
				`{"type":"response.output_item.added","output_index":0,"item":{"id":"msg_probe","type":"message","status":"in_progress","role":"assistant","content":[]}}`,
				`{"type":"response.output_text.delta","item_id":"msg_probe","output_index":0,"content_index":0,"delta":"PROBE_OK"}`,
				`{"type":"response.output_item.done","output_index":0,"item":{"id":"msg_probe","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"PROBE_OK","annotations":[]}]}}`,
				`{"type":"response.completed","response":{"id":"resp_probe_final","status":"completed","output":[{"id":"msg_probe","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"PROBE_OK","annotations":[]}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
			} {
				if err := conn.Write(ctx, coderws.MessageText, []byte(event)); err != nil {
					return
				}
			}
		}
	}))
	defer func() { cancel(); server.Close(); handlers.Wait() }()
	tmp := t.TempDir()
	args := []string{"exec", "--ignore-user-config", "--ignore-rules", "--skip-git-repo-check", "--ephemeral", "--json", "-s", "read-only", "-C", tmp, "-m", "gpt-5.1", "-c", `model_provider="probe"`, "-c", `model_providers.probe.name="Local verification"`, "-c", fmt.Sprintf("model_providers.probe.base_url=%q", server.URL+"/v1"), "-c", `model_providers.probe.wire_api="responses"`, "-c", `model_providers.probe.env_key="SUB2API_SYNTHETIC_KEY"`, "-c", `model_providers.probe.supports_websockets=true`, "--enable", "responses_websockets_v2", "Return PROBE_OK. SYNTHETIC_WS_RECOVERY_MARKER. Do not call tools."}
	prompt := args[len(args)-1]
	args = append(args[:len(args)-1], "-c", fmt.Sprintf("chatgpt_base_url=%q", server.URL+"/backend-api"), "-c", "features.plugins=false", prompt)
	command := exec.CommandContext(ctx, binary, args...)
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "CODEX_HOME=") && !strings.HasPrefix(value, "OPENAI_API_KEY=") {
			command.Env = append(command.Env, value)
		}
	}
	command.Env = append(command.Env, "CODEX_HOME="+tmp, "SUB2API_SYNTHETIC_KEY=synthetic-not-a-secret")
	command.Env = append(command.Env, "HTTP_PROXY=http://127.0.0.1:1", "HTTPS_PROXY=http://127.0.0.1:1", "http_proxy=http://127.0.0.1:1", "https_proxy=http://127.0.0.1:1", "NO_PROXY=127.0.0.1,localhost", "no_proxy=127.0.0.1,localhost")
	output, err := command.CombinedOutput()
	require.NoError(t, err, "isolated CLI: %s", string(output))
	mu.Lock()
	defer mu.Unlock()
	require.True(t, sentError)
	require.True(t, recovered)
	require.GreaterOrEqual(t, connections, 2)
	require.GreaterOrEqual(t, fullItems, incrementalItems)
	t.Logf("connections=%d incremental_items=%d full_items=%d recovered=%v", connections, incrementalItems, fullItems, recovered)
}
