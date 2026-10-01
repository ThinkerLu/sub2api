package main

//go:generate go run github.com/google/wire/cmd/wire

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	_ "github.com/Wei-Shaw/sub2api/ent/runtime"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/setup"
	"github.com/Wei-Shaw/sub2api/internal/web"

	"github.com/gin-gonic/gin"
)

//go:embed VERSION
var embeddedVersion string

// Build-time variables (can be set by ldflags)
var (
	Version   = ""
	Commit    = "unknown"
	Date      = "unknown"
	BuildType = "source" // "source" for manual builds, "release" for CI builds (set by ldflags)
)

func init() {
	// 如果 Version 已通过 ldflags 注入（例如 -X main.Version=...），则不要覆盖。
	if strings.TrimSpace(Version) != "" {
		return
	}

	// 默认从 embedded VERSION 文件读取版本号（编译期打包进二进制）。
	Version = strings.TrimSpace(embeddedVersion)
	if Version == "" {
		Version = "0.0.0-dev"
	}
}

// initLogger configures the default slog handler based on gin.Mode().
// In non-release mode, Debug level logs are enabled.
func main() {
	logger.InitBootstrap()
	defer logger.Sync()

	// Parse command line flags
	setupMode := flag.Bool("setup", false, "Run setup wizard in CLI mode")
	showVersion := flag.Bool("version", false, "Show version information")
	checkCodexV2Policy := flag.Bool("check-codex-v2-group-policy", false,
		"Validate the configured codex_v2_group_policy file and exit")
	checkCodexV2PolicyFile := flag.String("check-codex-v2-group-policy-file", "",
		"Validate a codex_v2_group_policy YAML file path and exit")
	flag.Parse()

	if *showVersion {
		log.Printf("Sub2API %s (commit: %s, built: %s)\n", Version, Commit, Date)
		return
	}

	// CLI setup mode
	if *setupMode {
		if err := setup.RunCLI(); err != nil {
			log.Fatalf("Setup failed: %v", err)
		}
		return
	}

	// Codex V2 分组策略预检：只读校验策略文件后退出，不启动服务、不连接数据库。
	if *checkCodexV2Policy || strings.TrimSpace(*checkCodexV2PolicyFile) != "" {
		if err := runCodexV2GroupPolicyCheck(strings.TrimSpace(*checkCodexV2PolicyFile)); err != nil {
			log.Fatalf("Codex V2 group policy check failed: %v", err)
		}
		return
	}

	// Check if setup is needed
	if setup.NeedsSetup() {
		// Check if auto-setup is enabled (for Docker deployment)
		if setup.AutoSetupEnabled() {
			log.Println("Auto setup mode enabled...")
			if err := setup.AutoSetupFromEnv(); err != nil {
				log.Fatalf("Auto setup failed: %v", err)
			}
			// Continue to main server after auto-setup
		} else {
			log.Println("First run detected, starting setup wizard...")
			runSetupServer()
			return
		}
	}

	// Normal server mode
	runMainServer()
}

// runCodexV2GroupPolicyCheck 只读校验策略文件：显式路径优先，否则读取配置里的
// codex_v2_group_policy.file。未配置路径时提示功能处于关闭状态并以成功退出。
// 输出只包含分组 ID、计数与摘要，不包含策略正文。
func runCodexV2GroupPolicyCheck(explicitFile string) error {
	path := strings.TrimSpace(explicitFile)
	if path == "" {
		cfg, err := config.LoadForBootstrap()
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}
		path = strings.TrimSpace(cfg.CodexV2GroupPolicy.File)
		if path == "" {
			log.Println("codex_v2_group_policy.file is not configured; all groups are disabled")
			return nil
		}
	}
	result, err := service.CheckCodexV2GroupPolicyFile(path)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return err
	}
	log.Printf("codex v2 group policy OK: %s", encoded)
	return nil
}

func runSetupServer() {
	r := gin.New()
	r.Use(middleware.Recovery())
	r.Use(middleware.CORS(config.CORSConfig{}))
	r.Use(middleware.SecurityHeaders(config.CSPConfig{Enabled: true, Policy: config.DefaultCSPPolicy}, nil))

	// Register setup routes
	setup.RegisterRoutes(r)

	// Serve embedded frontend if available
	if web.HasEmbeddedFrontend() {
		r.Use(web.ServeEmbeddedFrontend())
	}

	// Get server address from config.yaml or environment variables (SERVER_HOST, SERVER_PORT)
	// This allows users to run setup on a different address if needed
	addr := config.GetServerAddress()
	log.Printf("Setup wizard available at http://%s", addr)
	log.Println("Complete the setup wizard to configure Sub2API")

	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)

	server := &http.Server{
		Addr:              addr,
		Handler:           r,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
		Protocols:         protocols,
	}

	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("Failed to start setup server: %v", err)
	}
}

func runMainServer() {
	cfg, err := config.LoadForBootstrap()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	if err := logger.Init(logger.OptionsFromConfig(cfg.Log)); err != nil {
		log.Fatalf("Failed to initialize logger: %v", err)
	}
	modelRegistry, err := service.InitializeModelRegistry(cfg)
	if err != nil {
		log.Fatalf("Failed to initialize model registry: %v", err)
	}
	wsContextPolicy, err := service.InitializeOpenAIWSContextPolicy(cfg)
	if err != nil {
		log.Fatalf("Failed to initialize OpenAI WS context policy: %v", err)
	}
	// 策略文件未配置时该功能整体关闭；显式配置了路径而启动时无效则启动失败。
	codexV2GroupPolicy, err := service.InitializeCodexV2GroupPolicy(cfg)
	if err != nil {
		log.Fatalf("Failed to initialize Codex V2 group policy: %v", err)
	}
	defer codexV2GroupPolicy.Close()
	reloadQuit := make(chan struct{})
	defer close(reloadQuit)
	reloadSignal := make(chan os.Signal, 1)
	signal.Notify(reloadSignal, syscall.SIGHUP)
	defer signal.Stop(reloadSignal)
	go func() {
		for {
			select {
			case <-reloadSignal:
				if err := service.ReloadRuntimeConfigs(modelRegistry, wsContextPolicy); err != nil {
					log.Printf("Runtime config reload failed; keeping previous valid snapshots: %v", err)
				} else {
					log.Printf("Runtime config reload completed")
				}
				if err := codexV2GroupPolicy.Reload(); err != nil {
					log.Printf("Codex V2 group policy reload failed; keeping previous snapshot: %v", err)
				}
			case <-reloadQuit:
				return
			}
		}
	}()
	if cfg.RunMode == config.RunModeSimple {
		log.Println("⚠️  WARNING: Running in SIMPLE mode - billing and quota checks are DISABLED")
	}

	buildInfo := handler.BuildInfo{
		Version:   Version,
		BuildType: BuildType,
	}

	app, err := initializeApplication(buildInfo)
	if err != nil {
		log.Fatalf("Failed to initialize application: %v", err)
	}
	defer app.Cleanup()
	if app.PluginManager != nil {
		if err := app.PluginManager.Start(context.Background()); err != nil {
			log.Printf("Plugin manager started in degraded state: %v", err)
		}
	}
	if app.PromptAudit != nil {
		if err := app.PromptAudit.Start(context.Background()); err != nil {
			// Startup continues so unrelated APIs stay up. Fail-closed (unavailable)
			// applies only when a persisted blocking policy was observed; without
			// blocking intent, Prompt Audit stays ModeOff so the gateway remains
			// usable and administrators can still disable the feature (#4560).
			log.Printf("Prompt Audit started in degraded state: %v", err)
		}
	}

	// 启动服务器
	go func() {
		if err := app.Server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("Failed to start server: %v", err)
		}
	}()

	log.Printf("Server started on %s", app.Server.Addr)

	// 等待中断信号
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down server...")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := app.Server.Shutdown(ctx); err != nil {
		log.Printf("Server forced to shutdown: %v", err)
	}

	log.Println("Server exited")
}
