package handler

// Diagnostic overlay; production source is unchanged. Parent test process owns
// client + fake upstream, child process owns the real HTTP handler and forwarder.
import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/testutil"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

type wsMemorySample struct {
	Name                                                    string
	HeapAlloc, TotalAlloc, HeapInuse, HeapSys, HeapReleased uint64
	PeakHeap, MaxRSS                                        uint64
	NumGC                                                   uint32
	Goroutines                                              int
	ActiveHandlers                                          int64
	ForcedGC                                                bool
}

func wsDiagnosticInt(key string, fallback int) int {
	if v, e := strconv.Atoi(os.Getenv(key)); e == nil {
		return v
	}
	return fallback
}

func wsDiagnosticRSSMax() uint64 {
	var u syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &u) != nil {
		return 0
	}
	n := uint64(u.Maxrss)
	if runtime.GOOS == "linux" {
		n *= 1024
	}
	return n
}

func TestDiagnosticWSGateway(t *testing.T) {
	if os.Getenv("WS_DIAG_CHILD") != "1" {
		t.Skip("child process only")
	}
	gin.SetMode(gin.ReleaseMode)
	runtime.MemProfileRate = 64 << 10
	dir := os.Getenv("WS_DIAG_DIR")
	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Default.RateMultiplier = 1
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Gateway.OpenAIWS.Enabled = os.Getenv("HTTP_DIAG_WS") != ""
	cfg.Gateway.MaxBodySize = 128 << 20
	cfg.Gateway.MaxLineSize = 32 << 20
	cfg.Gateway.OpenAIWS.OAuthEnabled = true
	cfg.Gateway.OpenAIWS.APIKeyEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
	cfg.Gateway.OpenAIWS.IngressModeDefault = service.OpenAIWSIngressModePassthrough
	cfg.Gateway.OpenAIWS.HTTPBridgeEnabled = os.Getenv("HTTP_DIAG_WS") == "bridge"
	cfg.Gateway.OpenAIWS.HTTPBridgeThresholdBytes = 4 << 20
	cfg.Gateway.OpenAIWS.ClientReadLimitBytes = 64 << 20
	cfg.Gateway.OpenAIWS.ClientFirstMessageTimeoutSeconds = 30
	cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 5
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 60
	cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 60
	cfg.Gateway.OpenAIWS.IngressInterTurnIdleTimeoutSeconds = 60
	cfg.Gateway.OpenAIFirstOutputTimeoutSeconds = 60
	account := service.Account{ID: 1, Name: "synthetic-local-only", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Status: service.StatusActive, Schedulable: true, Concurrency: 8,
		Credentials: map[string]any{"access_token": "synthetic-local-only", "chatgpt_account_id": "synthetic"},
		Extra:       map[string]any{"openai_passthrough": true, "openai_oauth_responses_websockets_v2_enabled": true, "openai_oauth_responses_websockets_v2_mode": service.OpenAIWSIngressModePassthrough},
	}
	account.Extra = map[string]any{"openai_passthrough": os.Getenv("HTTP_DIAG_ACCOUNT") != "oauth-normal"}
	if strings.HasPrefix(os.Getenv("HTTP_DIAG_ACCOUNT"), "apikey") {
		account.Type = service.AccountTypeAPIKey
		account.Credentials = map[string]any{"api_key": "synthetic-local-only"}
	}
	cache := testutil.NewRedisGatewayCache(t)
	billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	defer billing.Stop()
	svc := service.NewOpenAIGatewayService(&openAIWSFailoverHandlerAccountRepoStub{accounts: []service.Account{account}}, &openAIWSUsageHandlerUsageLogRepoStub{}, nil, nil, nil, nil, cache, cfg, nil, nil, service.NewBillingService(cfg, nil), nil, billing, &httpMemoryUpstream{target: os.Getenv("WS_DIAG_UPSTREAM")}, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil)
	service.DiagnosticWSRedirectUpstream(svc, "ws"+strings.TrimPrefix(os.Getenv("WS_DIAG_UPSTREAM"), "http"))
	cc := &concurrencyCacheMock{
		acquireUserSlotFn:    func(context.Context, int64, int, string) (bool, error) { return true, nil },
		acquireAccountSlotFn: func(context.Context, int64, int, string) (bool, error) { return true, nil },
	}
	h := NewOpenAIGatewayHandler(svc, service.NewConcurrencyService(cc), billing, service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, nil, cfg)
	var active atomic.Int64
	var peak atomic.Uint64
	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(5 * time.Millisecond)
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
	defer close(stop)
	router := gin.New()
	router.POST("/v1/responses", func(c *gin.Context) {
		groupID := int64(1)
		c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: 1, GroupID: &groupID, User: &service.User{ID: 2, Status: service.StatusActive}, Group: &service.Group{ID: 1, Platform: service.PlatformOpenAI, Status: service.StatusActive}})
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 2, Concurrency: 8})
		active.Add(1)
		defer active.Add(-1)
		h.Responses(c)
	})
	router.GET("/v1/responses", func(c *gin.Context) {
		groupID := int64(1)
		c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: 1, GroupID: &groupID, User: &service.User{ID: 2, Status: service.StatusActive}, Group: &service.Group{ID: 1, Platform: service.PlatformOpenAI, Status: service.StatusActive}})
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 2, Concurrency: 8})
		active.Add(1)
		defer active.Add(-1)
		h.ResponsesWebSocket(c)
	})
	router.GET("/diag", func(c *gin.Context) {
		name := c.Query("name")
		gc := c.Query("gc") == "1"
		if gc {
			runtime.GC()
			runtime.GC()
		}
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		if name == "baseline" {
			peak.Store(m.HeapAlloc)
		}
		s := wsMemorySample{Name: name, HeapAlloc: m.HeapAlloc, TotalAlloc: m.TotalAlloc, HeapInuse: m.HeapInuse, HeapSys: m.HeapSys, HeapReleased: m.HeapReleased, PeakHeap: peak.Load(), MaxRSS: wsDiagnosticRSSMax(), NumGC: m.NumGC, Goroutines: runtime.NumGoroutine(), ActiveHandlers: active.Load(), ForcedGC: gc}
		if c.Query("profile") == "1" {
			f, e := os.Create(filepath.Join(dir, name+".pprof"))
			if e == nil {
				_ = pprof.WriteHeapProfile(f)
				_ = f.Close()
			}
		}
		c.JSON(200, s)
	})
	shutdown := make(chan struct{}, 1)
	router.POST("/stop", func(c *gin.Context) {
		c.Status(200)
		select {
		case shutdown <- struct{}{}:
		default:
		}
	})
	server := httptest.NewServer(router)
	defer server.Close()
	ready, _ := json.Marshal(map[string]any{"url": server.URL, "pid": os.Getpid()})
	if e := os.WriteFile(filepath.Join(dir, "ready.json"), ready, 0600); e != nil {
		t.Fatal(e)
	}
	select {
	case <-shutdown:
	case <-time.After(240 * time.Second):
		t.Fatal("gateway timeout")
	}
}

type httpMemoryUpstream struct{ target string }

func (u *httpMemoryUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	clone := req.Clone(req.Context())
	target, err := url.Parse(u.target)
	if err != nil {
		return nil, err
	}
	clone.URL = target
	clone.Host = target.Host
	return http.DefaultClient.Do(clone)
}
func (u *httpMemoryUpstream) DoWithTLS(req *http.Request, p string, a int64, c int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, p, a, c)
}

func TestDiagnosticHTTPMemory(t *testing.T) {
	dir := os.Getenv("WS_DIAG_DIR")
	if dir == "" {
		t.Skip("explicit diagnostic only")
	}
	size := wsDiagnosticInt("HTTP_DIAG_BYTES", 8<<20)
	responseSize := wsDiagnosticInt("HTTP_DIAG_RESPONSE_BYTES", 0)
	stream := os.Getenv("HTTP_DIAG_STREAM") != "0"
	abort := os.Getenv("HTTP_DIAG_ABORT") == "1"
	forensic := os.Getenv("WS_DIAG_GC") == "1"
	wsMode := os.Getenv("HTTP_DIAG_WS")
	var wsAttempts atomic.Int64
	payload := []byte(fmt.Sprintf(`{"model":"gpt-5.4","instructions":"synthetic memory diagnostic","stream":%t,"store":false,"prompt_cache_key":"synthetic-session","tools":[{"type":"function","name":"test_tool","parameters":{"type":"object","properties":{}}}],"input":[{"role":"user","type":"message","content":[{"type":"input_image","image_url":"data:image/png;base64,%s"}]}]}`, stream, strings.Repeat("A", size)))
	if path := os.Getenv("HTTP_DIAG_PAYLOAD"); path != "" {
		var err error
		payload, err = os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	hash := httpMemoryInputHash(payload)
	receipt := make(chan string, 1)
	release := make(chan struct{})
	serverFinished := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			wsAttempts.Add(1)
			http.Error(w, "synthetic WS unavailable", 503)
			return
		}
		defer func() { serverFinished <- struct{}{} }()
		body, err := io.ReadAll(r.Body)
		if err != nil {
			receipt <- err.Error()
			return
		}
		if os.Getenv("HTTP_DIAG_HISTORY") == "1" {
			// Preserve full normalized input for cross-version hash comparison.
			full := httpMemoryCanonicalInputHash(body)
			_ = os.WriteFile(filepath.Join(dir, "upstream-input.sha256"), []byte(hex.EncodeToString(full[:])), 0600)
		}
		actual := httpMemoryInputHash(body)
		receipt <- hex.EncodeToString(actual[:])
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_http_diag\",\"model\":\"gpt-5.4\"}}\n\n")
			w.(http.Flusher).Flush()
		}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		if stream {
			if responseSize > 0 {
				fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"%s\"}\n\n", strings.Repeat("R", responseSize))
				w.(http.Flusher).Flush()
			}
			fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_http_diag\",\"status\":\"completed\",\"model\":\"gpt-5.4\",\"output\":[],\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}}\n\ndata: [DONE]\n\n")
		} else {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id":"resp_http_diag","status":"completed","model":"gpt-5.4","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"%s"}]}],"usage":{"input_tokens":3,"output_tokens":1}}`, strings.Repeat("R", responseSize))
		}
	}))
	defer upstream.Close()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestDiagnosticWSGateway$", "-test.v", "-test.timeout=120s")
	cmd.Env = append(os.Environ(), "WS_DIAG_CHILD=1", "WS_DIAG_UPSTREAM="+upstream.URL)
	log, err := os.Create(filepath.Join(dir, "gateway.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	cmd.Stdout = log
	cmd.Stderr = log
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	var ready struct {
		URL string
		PID int
	}
	deadline := time.Now().Add(15 * time.Second)
	for ready.URL == "" && time.Now().Before(deadline) {
		data, _ := os.ReadFile(filepath.Join(dir, "ready.json"))
		_ = json.Unmarshal(data, &ready)
		if ready.URL == "" {
			time.Sleep(25 * time.Millisecond)
		}
	}
	if ready.URL == "" {
		t.Fatal("gateway readiness failed")
	}
	var samples []wsMemorySample
	sample := func(name string, gc bool) wsMemorySample {
		url := ready.URL + "/diag?name=" + name
		if gc {
			url += "&gc=1"
		}
		if forensic && gc {
			url += "&profile=1"
		}
		resp, e := http.Get(url)
		if e != nil {
			t.Fatal(e)
		}
		defer resp.Body.Close()
		var s wsMemorySample
		if e = json.NewDecoder(resp.Body).Decode(&s); e != nil {
			t.Fatal(e)
		}
		samples = append(samples, s)
		return s
	}
	base := sample("baseline", true)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", ready.URL+"/v1/responses", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "codex_cli_rs/0.0.0")
	req.Header.Set("session_id", "synthetic-session")
	if wsMode == "fallback" {
		conn, _, e := coderws.Dial(ctx, "ws"+strings.TrimPrefix(ready.URL, "http")+"/v1/responses", &coderws.DialOptions{HTTPHeader: req.Header})
		if e != nil {
			t.Fatal(e)
		}
		first := append([]byte(`{"type":"response.create",`), payload[1:]...)
		if e = conn.Write(ctx, coderws.MessageText, first); e != nil {
			t.Fatal(e)
		}
		for {
			_, _, e = conn.Read(ctx)
			if e != nil {
				break
			}
		}
		_ = conn.CloseNow()
		if wsAttempts.Load() == 0 {
			t.Fatal("fallback fixture never attempted upstream WS")
		}
		sample("ws_failed_raw", false)
		if forensic {
			sample("ws_failed_gc", true)
		}
	}
	outcome := make(chan error, 1)
	go func() {
		if wsMode == "bridge" {
			conn, _, e := coderws.Dial(ctx, "ws"+strings.TrimPrefix(ready.URL, "http")+"/v1/responses", &coderws.DialOptions{HTTPHeader: req.Header})
			if e != nil {
				outcome <- e
				return
			}
			defer conn.CloseNow()
			first := append([]byte(`{"type":"response.create",`), payload[1:]...)
			if e = conn.Write(ctx, coderws.MessageText, first); e != nil {
				outcome <- e
				return
			}
			for {
				_, message, e := conn.Read(ctx)
				if e != nil {
					outcome <- e
					return
				}
				if gjson.GetBytes(message, "type").String() == "response.completed" {
					outcome <- nil
					return
				}
				if gjson.GetBytes(message, "type").String() == "error" {
					outcome <- fmt.Errorf("bridge failed: %.400s", message)
					return
				}
			}
		}
		resp, e := http.DefaultClient.Do(req)
		if e != nil {
			outcome <- e
			return
		}
		defer resp.Body.Close()
		body, e := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			e = fmt.Errorf("HTTP %d: %.400s", resp.StatusCode, body)
		}
		if !abort && e == nil {
			if stream && !bytes.Contains(body, []byte("response.completed")) {
				e = fmt.Errorf("terminal event missing: %.400s", body)
			}
			if responseSize > 0 && !bytes.Contains(body, []byte(strings.Repeat("R", responseSize))) {
				e = fmt.Errorf("response content missing")
			}
		}
		outcome <- e
	}()
	select {
	case got := <-receipt:
		if got != hex.EncodeToString(hash[:]) {
			t.Fatalf("upstream input changed: %s", got)
		}
	case e := <-outcome:
		t.Fatalf("request failed before upstream: %v", e)
	case <-ctx.Done():
		t.Fatal("upstream timeout")
	}
	time.Sleep(100 * time.Millisecond)
	sample("upstream_wait_raw", false)
	if forensic {
		sample("upstream_wait_gc", true)
	}
	if abort {
		cancel()
	}
	close(release)
	select {
	case e := <-outcome:
		if !abort && e != nil {
			t.Fatal(e)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("client finish timeout")
	}
	select {
	case <-serverFinished:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream not canceled/finished")
	}
	for i := 0; i < 30; i++ {
		s := sample("completed_raw", false)
		if s.ActiveHandlers == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	final := sample("completed_gc", true)
	if final.ActiveHandlers != 0 {
		t.Fatal("gateway handler did not finish")
	}
	if wsMode == "bridge" && wsAttempts.Load() != 0 {
		t.Fatal("threshold bridge unexpectedly dialed WS")
	}
	result := map[string]any{"ws_mode": wsMode, "upstream_ws_attempts": wsAttempts.Load(), "account": os.Getenv("HTTP_DIAG_ACCOUNT"), "request_bytes": len(payload), "response_bytes": responseSize, "stream": stream, "abort": abort, "forensic": forensic, "input_sha256": hex.EncodeToString(hash[:]), "total_alloc_delta": final.TotalAlloc - base.TotalAlloc, "peak_heap_delta": final.PeakHeap - base.HeapAlloc, "retained_after_complete": int64(final.HeapAlloc) - int64(base.HeapAlloc), "samples": samples}
	data, _ := json.MarshalIndent(result, "", "  ")
	if err = os.WriteFile(filepath.Join(dir, "result.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	resp, e := http.Post(ready.URL+"/stop", "text/plain", nil)
	if e == nil {
		resp.Body.Close()
	}
	if e = cmd.Wait(); e != nil {
		t.Fatal(e)
	}
}

func httpMemoryInputHash(body []byte) [32]byte {
	if os.Getenv("HTTP_DIAG_HISTORY") != "1" {
		return httpMemoryCanonicalInputHash(body)
	}
	var images []string
	var walk func(gjson.Result)
	walk = func(v gjson.Result) {
		if v.IsObject() && v.Get("type").String() == "input_image" {
			images = append(images, v.Get("image_url").String())
			return
		}
		if v.IsObject() || v.IsArray() {
			v.ForEach(func(_, child gjson.Result) bool { walk(child); return true })
		}
	}
	walk(gjson.GetBytes(body, "input"))
	if len(images) == 0 {
		panic("history fixture has no images")
	}
	raw, _ := json.Marshal(images)
	return sha256.Sum256(raw)
}

func httpMemoryCanonicalInputHash(body []byte) [32]byte {
	var value any
	dec := json.NewDecoder(strings.NewReader(gjson.GetBytes(body, "input").Raw))
	dec.UseNumber()
	if err := dec.Decode(&value); err != nil {
		panic(err)
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return sha256.Sum256(canonical)
}
