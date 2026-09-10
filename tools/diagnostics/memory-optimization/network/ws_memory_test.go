package handler

// Diagnostic overlay; production source is unchanged. Parent test process owns
// client + fake upstream, child process owns the real handler and WS forwarder.
import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.OAuthEnabled = true
	cfg.Gateway.OpenAIWS.APIKeyEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
	cfg.Gateway.OpenAIWS.IngressModeDefault = service.OpenAIWSIngressModePassthrough
	cfg.Gateway.OpenAIWS.HTTPBridgeEnabled = false // test pure passthrough explicitly
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
	cache := testutil.NewRedisGatewayCache(t)
	billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	defer billing.Stop()
	svc := service.NewOpenAIGatewayService(&openAIWSFailoverHandlerAccountRepoStub{accounts: []service.Account{account}}, &openAIWSUsageHandlerUsageLogRepoStub{}, nil, nil, nil, nil, cache, cfg, nil, nil, service.NewBillingService(cfg, nil), nil, billing, nil, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil)
	service.DiagnosticWSRedirectUpstream(svc, os.Getenv("WS_DIAG_UPSTREAM"))
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

type wsDiagnosticRepeat struct {
	remaining int64
	random    uint64
}

func (r *wsDiagnosticRepeat) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n := len(p)
	if int64(n) > r.remaining {
		n = int(r.remaining)
	}
	for i := 0; i < n; i++ {
		if r.random == 0 {
			p[i] = 'A'
		} else {
			x := r.random
			x ^= x << 13
			x ^= x >> 7
			x ^= x << 17
			r.random = x
			p[i] = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"[x&63]
		}
	}
	r.remaining -= int64(n)
	return n, nil
}

func wsDiagnosticBody(size int, kind string, turn int) io.Reader {
	prev := ""
	if turn > 1 {
		prev = fmt.Sprintf(`,"previous_response_id":"resp_diag_%d"`, turn-1)
	}
	prefix := `{"type":"response.create","model":"gpt-5.4","instructions":"synthetic memory diagnostic","stream":true,"store":false,"prompt_cache_key":"synthetic-local-session"` + prev + `,"tools":[{"type":"function","name":"test_tool","parameters":{"type":"object","properties":{}}}],"input":[`
	suffix := `"}]}]}`
	if kind == "encrypted" {
		prefix += `{"type":"reasoning","encrypted_content":"`
		suffix = `"}]}`
	} else if kind == "text" {
		prefix += `{"role":"user","type":"message","content":[{"type":"input_text","text":"`
	} else {
		prefix += `{"role":"user","type":"message","content":[{"type":"input_image","image_url":"data:image/png;base64,`
	}
	if kind == "identity" {
		prefix = strings.Replace(prefix, `"stream":true`, `"client_metadata":{"session_id":"synthetic-local-session"},"stream":true`, 1)
	}
	seed := uint64(0)
	if os.Getenv("WS_DIAG_ENTROPY") == "1" {
		seed = 0x243f6a8885a308d3
	}
	return io.MultiReader(strings.NewReader(prefix), &wsDiagnosticRepeat{remaining: int64(size - len(prefix) - len(suffix)), random: seed}, strings.NewReader(suffix))
}

type wsDiagnosticTurn struct {
	Number      int
	Bytes       int64
	SHA256      string
	InputSHA256 string
	Done        chan struct{}
	Err         error
}

func TestDiagnosticWSPassthroughMemory(t *testing.T) {
	if os.Getenv("WS_DIAG_DIR") == "" {
		t.Skip("diagnostic only")
	}
	dir := os.Getenv("WS_DIAG_DIR")
	_ = os.MkdirAll(dir, 0700)
	size := wsDiagnosticInt("WS_DIAG_BYTES", 62134119)
	rounds := wsDiagnosticInt("WS_DIAG_ROUNDS", 1)
	var sizes []int
	if raw := os.Getenv("WS_DIAG_SIZES"); raw != "" {
		for _, v := range strings.Split(raw, ",") {
			n, e := strconv.Atoi(v)
			if e != nil || n < 1024 {
				t.Fatal("invalid sizes")
			}
			sizes = append(sizes, n)
		}
		rounds = len(sizes)
	}
	kind := os.Getenv("WS_DIAG_KIND")
	if kind == "" {
		kind = "image"
	}
	compressed := os.Getenv("WS_DIAG_COMPRESSION") != "0"
	forensic := os.Getenv("WS_DIAG_GC") == "1"
	upstreamTurns := make(chan wsDiagnosticTurn, 16)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mode := coderws.CompressionDisabled
		if compressed {
			mode = coderws.CompressionContextTakeover
		}
		conn, err := coderws.Accept(w, r, &coderws.AcceptOptions{CompressionMode: mode})
		if err != nil {
			upstreamTurns <- wsDiagnosticTurn{Err: err}
			return
		}
		defer conn.CloseNow()
		conn.SetReadLimit(64 << 20)
		for turn := 1; ; turn++ {
			ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
			_, reader, e := conn.Reader(ctx)
			if e != nil {
				cancel()
				return
			}
			body, e := io.ReadAll(reader)
			n := int64(len(body))
			cancel()
			if e != nil {
				upstreamTurns <- wsDiagnosticTurn{Err: e}
				return
			}
			done := make(chan struct{})
			hash := sha256.Sum256(body)
			inputHash := sha256.Sum256([]byte(gjson.GetBytes(body, "input").Raw))
			body = nil
			upstreamTurns <- wsDiagnosticTurn{Number: turn, Bytes: n, SHA256: hex.EncodeToString(hash[:]), InputSHA256: hex.EncodeToString(inputHash[:]), Done: done}
			ctx, cancel = context.WithTimeout(r.Context(), 120*time.Second)
			e = conn.Write(ctx, coderws.MessageText, []byte(fmt.Sprintf(`{"type":"response.created","response":{"id":"resp_diag_%d","model":"gpt-5.4","status":"in_progress"}}`, turn)))
			if e == nil {
				e = conn.Write(ctx, coderws.MessageText, []byte(`{"type":"response.output_text.delta","delta":"ok"}`))
			}
			if e != nil {
				cancel()
				return
			}
			select {
			case <-done:
			case <-ctx.Done():
				cancel()
				return
			}
			e = conn.Write(ctx, coderws.MessageText, []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_diag_%d","model":"gpt-5.4","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`, turn)))
			cancel()
			if e != nil {
				return
			}
		}
	}))
	defer upstream.Close()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestDiagnosticWSGateway$", "-test.v", "-test.timeout=250s")
	cmd.Env = append(os.Environ(), "WS_DIAG_CHILD=1", "WS_DIAG_UPSTREAM="+"ws"+strings.TrimPrefix(upstream.URL, "http"))
	logfile, err := os.Create(filepath.Join(dir, "gateway.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer logfile.Close()
	cmd.Stdout = logfile
	cmd.Stderr = logfile
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}()
	var ready struct {
		URL string
		PID int
	}
	deadline := time.Now().Add(15 * time.Second)
	for ready.URL == "" && time.Now().Before(deadline) {
		data, e := os.ReadFile(filepath.Join(dir, "ready.json"))
		if e == nil {
			_ = json.Unmarshal(data, &ready)
		}
		if ready.URL == "" {
			time.Sleep(25 * time.Millisecond)
		}
	}
	if ready.URL == "" {
		t.Fatal("gateway readiness timeout; see gateway.log")
	}
	var samples []wsMemorySample
	sample := func(name string, gc bool) wsMemorySample {
		u := ready.URL + "/diag?name=" + name
		if gc {
			u += "&gc=1"
		}
		if forensic && gc {
			u += "&profile=1"
		}
		resp, e := http.Get(u)
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
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	mode := coderws.CompressionDisabled
	if compressed {
		mode = coderws.CompressionContextTakeover
	}
	conn, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(ready.URL, "http")+"/v1/responses", &coderws.DialOptions{CompressionMode: mode, HTTPHeader: http.Header{"User-Agent": []string{"codex_cli_rs/0.0.0"}, "session_id": []string{"synthetic-local-session"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	var turns []map[string]any
	started := time.Now()
	for turn := 1; turn <= rounds; turn++ {
		turnSize := size
		if turn > 1 && os.Getenv("WS_DIAG_DELTA") == "1" {
			turnSize = 4096
		}
		if len(sizes) > 0 {
			turnSize = sizes[turn-1]
		}
		wantBody, _ := io.ReadAll(wsDiagnosticBody(turnSize, kind, turn))
		wantInput := sha256.Sum256([]byte(gjson.GetBytes(wantBody, "input").Raw))
		wantBody = nil
		start := time.Now()
		wr, e := conn.Writer(ctx, coderws.MessageText)
		if e != nil {
			t.Fatal(e)
		}
		_, e = io.Copy(wr, wsDiagnosticBody(turnSize, kind, turn))
		if e != nil {
			t.Fatal(e)
		}
		if e = wr.Close(); e != nil {
			t.Fatal(e)
		}
		var receipt wsDiagnosticTurn
		select {
		case receipt = <-upstreamTurns:
		case <-ctx.Done():
			t.Fatal("upstream did not receive request")
		}
		if receipt.Err != nil {
			t.Fatal(receipt.Err)
		}
		// The normalizer may add small top-level/tool metadata. Require the
		// complete input, including large opaque content, to remain identical.
		if receipt.InputSHA256 != hex.EncodeToString(wantInput[:]) || receipt.Bytes < int64(turnSize)-1024 || receipt.Bytes > int64(turnSize)+4096 {
			t.Fatalf("upstream input changed: bytes=%d want=%d input_hash=%s want=%x", receipt.Bytes, turnSize, receipt.InputSHA256, wantInput)
		}
		for {
			_, p, e := conn.Read(ctx)
			if e != nil {
				t.Fatal(e)
			}
			if strings.Contains(string(p), "response.output_text.delta") {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
		sample(fmt.Sprintf("turn%d_stream_raw", turn), false)
		if forensic {
			sample(fmt.Sprintf("turn%d_stream_gc", turn), true)
		}
		time.Sleep(time.Duration(wsDiagnosticInt("WS_DIAG_WAIT_MS", 1000)) * time.Millisecond)
		close(receipt.Done)
		for {
			_, p, e := conn.Read(ctx)
			if e != nil {
				t.Fatal(e)
			}
			if strings.Contains(string(p), "response.completed") {
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
		sample(fmt.Sprintf("turn%d_idle_raw", turn), false)
		if forensic {
			sample(fmt.Sprintf("turn%d_idle_gc", turn), true)
		}
		turns = append(turns, map[string]any{"turn": turn, "request_bytes": turnSize, "upstream_bytes": receipt.Bytes, "upstream_sha256": receipt.SHA256, "input_sha256_verified": true, "elapsed_ms": time.Since(start).Milliseconds()})
	}
	sample("idle_1s_raw", false)
	time.Sleep(time.Second)
	if forensic {
		sample("idle_1s_gc", true)
	}
	_ = conn.CloseNow()
	var closed wsMemorySample
	for i := 0; i < 40; i++ {
		time.Sleep(50 * time.Millisecond)
		closed = sample("closed_raw", false)
		if closed.ActiveHandlers == 0 {
			break
		}
	}
	if closed.ActiveHandlers != 0 {
		t.Fatal("gateway handler did not exit")
	}
	closed = sample("closed_gc", true)
	resp, e := http.Post(ready.URL+"/stop", "text/plain", nil)
	if e == nil {
		_ = resp.Body.Close()
	}
	if e = cmd.Wait(); e != nil {
		t.Fatalf("gateway failed: %v", e)
	}
	result := map[string]any{"kind": kind, "body_bytes": size, "rounds": rounds, "delta": os.Getenv("WS_DIAG_DELTA") == "1", "high_entropy": os.Getenv("WS_DIAG_ENTROPY") == "1", "compression_negotiated": compressed, "force_gc_at_stages": forensic, "gateway_pid": ready.PID, "elapsed_ms": time.Since(started).Milliseconds(), "total_alloc_delta": closed.TotalAlloc - base.TotalAlloc, "peak_heap_delta": closed.PeakHeap - base.HeapAlloc, "retained_heap_after_close": int64(closed.HeapAlloc) - int64(base.HeapAlloc), "samples": samples, "turns": turns}
	encoded, _ := json.MarshalIndent(result, "", "  ")
	_ = os.WriteFile(filepath.Join(dir, "result.json"), encoded, 0600)
	t.Log(string(encoded))
	// Diagnostic regression signal: one turn must fit 4 body sizes of heap
	// above baseline, and a closed connection must not retain half a large body.
	if bound := wsDiagnosticInt("WS_DIAG_MAX_HEAP_MULTIPLIER", 0); bound > 0 && closed.PeakHeap-base.HeapAlloc > uint64(bound*size) {
		t.Errorf("heap amplification: peak delta %.1f MiB exceeds %dx body", float64(closed.PeakHeap-base.HeapAlloc)/(1<<20), bound)
	}
	if size >= 8<<20 && closed.HeapAlloc > base.HeapAlloc+uint64(size/2) {
		t.Errorf("body-sized memory remains after connection close and GC")
	}
}
