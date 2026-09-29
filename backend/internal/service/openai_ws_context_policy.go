package service

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"gopkg.in/yaml.v3"
)

const openAIWSContextPolicyMaxBytes int64 = 1 << 20

type openAIWSContextPolicySnapshot struct {
	accounts     map[string]struct{}
	sourceSHA256 string
	loadedAt     time.Time
}

type openAIWSContextPolicyDocument struct {
	Version  int                                      `yaml:"version"`
	Accounts *map[string]openAIWSContextPolicyAccount `yaml:"accounts"`
}

// openAIWSContextPolicyAccount reserves a per-account object for future policy
// fields while rejecting misspelled or unsupported options today.
type openAIWSContextPolicyAccount struct{}

// OpenAIWSContextPolicy owns an immutable, atomically replaceable set of
// accounts that should retain adaptive WS history only for HTTP bridge turns.
// Reload failures never replace the last valid snapshot.
type OpenAIWSContextPolicy struct {
	enabled        bool
	path           string
	reloadInterval time.Duration
	snapshot       atomic.Pointer[openAIWSContextPolicySnapshot]
	reloadMu       sync.Mutex
	startOnce      sync.Once
	stopOnce       sync.Once
	stopCh         chan struct{}
	doneCh         chan struct{}
	started        atomic.Bool
	lastErrorMu    sync.Mutex
	lastError      string
}

var processOpenAIWSContextPolicy atomic.Pointer[OpenAIWSContextPolicy]

// CurrentOpenAIWSContextPolicy returns the process-level policy. A nil result
// means the feature has not been initialized, which preserves legacy behavior.
func CurrentOpenAIWSContextPolicy() *OpenAIWSContextPolicy {
	return processOpenAIWSContextPolicy.Load()
}

// InitializeOpenAIWSContextPolicy loads and publishes the process-level policy.
// When enabled, the initial file must be valid; startup fails instead of
// silently applying a partial or unintended account scope.
func InitializeOpenAIWSContextPolicy(cfg *config.Config) (*OpenAIWSContextPolicy, error) {
	policy := &OpenAIWSContextPolicy{stopCh: make(chan struct{}), doneCh: make(chan struct{})}
	if cfg != nil && cfg.Gateway.OpenAIWS.ContextPolicy.Enabled {
		policy.enabled = true
		policy.path = strings.TrimSpace(cfg.Gateway.OpenAIWS.ContextPolicy.File)
		policy.reloadInterval = time.Duration(cfg.Gateway.OpenAIWS.ContextPolicy.ReloadIntervalSeconds) * time.Second
		if policy.path == "" {
			return nil, errors.New("gateway.openai_ws.context_policy.file is required when context_policy.enabled is true")
		}
		if err := policy.Reload(); err != nil {
			return nil, fmt.Errorf("load OpenAI WS context policy: %w", err)
		}
	}
	processOpenAIWSContextPolicy.Store(policy)
	policy.Start()
	return policy, nil
}

// SetCurrentOpenAIWSContextPolicyForTest swaps the process-level policy and
// returns a restore function. It keeps tests isolated without exposing mutable
// state.
func SetCurrentOpenAIWSContextPolicyForTest(policy *OpenAIWSContextPolicy) func() {
	previous := processOpenAIWSContextPolicy.Swap(policy)
	return func() { processOpenAIWSContextPolicy.Store(previous) }
}

func NewOpenAIWSContextPolicyFromBytes(data []byte) (*OpenAIWSContextPolicy, error) {
	snapshot, err := parseOpenAIWSContextPolicySnapshot(data)
	if err != nil {
		return nil, err
	}
	policy := &OpenAIWSContextPolicy{enabled: true, stopCh: make(chan struct{}), doneCh: make(chan struct{})}
	policy.snapshot.Store(snapshot)
	return policy, nil
}

// ShouldRetainAdaptiveHistory reports whether a completed adaptive turn should
// remain part of the connection's replay history. The default is true so a
// disabled policy, a missing snapshot, or a non-matching account preserves the
// existing accumulation behavior.
func (p *OpenAIWSContextPolicy) ShouldRetainAdaptiveHistory(accountName string, usedHTTPBridge bool) bool {
	if !p.AppliesTo(accountName) {
		return true
	}
	return usedHTTPBridge
}

// AppliesTo reports whether the enabled policy scopes the named account.
func (p *OpenAIWSContextPolicy) AppliesTo(accountName string) bool {
	if p == nil || !p.enabled {
		return false
	}
	snapshot := p.snapshot.Load()
	if snapshot == nil {
		return false
	}
	if len(snapshot.accounts) > 0 {
		if _, ok := snapshot.accounts[strings.TrimSpace(accountName)]; !ok {
			return false
		}
	}
	return true
}

func (p *OpenAIWSContextPolicy) Start() {
	if p == nil || !p.enabled || p.reloadInterval <= 0 {
		return
	}
	p.startOnce.Do(func() {
		p.started.Store(true)
		go func() {
			defer close(p.doneCh)
			ticker := time.NewTicker(p.reloadInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					if err := p.Reload(); err != nil {
						p.logReloadError(err)
					}
				case <-p.stopCh:
					return
				}
			}
		}()
		log.Printf("[OpenAIWSContextPolicy] watching %s every %s", p.path, p.reloadInterval)
	})
}

func (p *OpenAIWSContextPolicy) Close() {
	if p == nil || !p.started.Load() {
		return
	}
	p.stopOnce.Do(func() { close(p.stopCh) })
	<-p.doneCh
}

func (p *OpenAIWSContextPolicy) Reload() error {
	if p == nil || !p.enabled {
		return nil
	}
	p.reloadMu.Lock()
	defer p.reloadMu.Unlock()

	info, err := os.Stat(p.path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", p.path, err)
	}
	if info.Size() > openAIWSContextPolicyMaxBytes {
		return fmt.Errorf("OpenAI WS context policy exceeds %d bytes", openAIWSContextPolicyMaxBytes)
	}
	data, err := os.ReadFile(p.path)
	if err != nil {
		return fmt.Errorf("read %s: %w", p.path, err)
	}
	digest := sha256.Sum256(data)
	hash := hex.EncodeToString(digest[:])
	if current := p.snapshot.Load(); current != nil && current.sourceSHA256 == hash {
		p.clearReloadError()
		return nil
	}
	snapshot, err := parseOpenAIWSContextPolicySnapshot(data)
	if err != nil {
		return err
	}
	snapshot.sourceSHA256 = hash
	snapshot.loadedAt = time.Now().UTC()
	p.snapshot.Store(snapshot)
	p.clearReloadError()
	log.Printf("[OpenAIWSContextPolicy] loaded %d scoped accounts from %s", len(snapshot.accounts), p.path)
	return nil
}

func (p *OpenAIWSContextPolicy) logReloadError(err error) {
	if p == nil || err == nil {
		return
	}
	message := err.Error()
	p.lastErrorMu.Lock()
	defer p.lastErrorMu.Unlock()
	if message == p.lastError {
		return
	}
	p.lastError = message
	log.Printf("[OpenAIWSContextPolicy] reload failed; keeping previous snapshot: %v", err)
}

func (p *OpenAIWSContextPolicy) clearReloadError() {
	p.lastErrorMu.Lock()
	p.lastError = ""
	p.lastErrorMu.Unlock()
}

func parseOpenAIWSContextPolicySnapshot(data []byte) (*openAIWSContextPolicySnapshot, error) {
	var document openAIWSContextPolicyDocument
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("parse OpenAI WS context policy: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse trailing OpenAI WS context policy content: %w", err)
	} else if err == nil {
		return nil, errors.New("OpenAI WS context policy must contain one YAML document")
	}
	if document.Version != 1 {
		return nil, fmt.Errorf("unsupported OpenAI WS context policy version %d", document.Version)
	}
	if document.Accounts == nil {
		return nil, errors.New("OpenAI WS context policy accounts field is required")
	}
	accounts := make(map[string]struct{}, len(*document.Accounts))
	for rawName := range *document.Accounts {
		name := strings.TrimSpace(rawName)
		if name == "" {
			return nil, errors.New("OpenAI WS context policy account name cannot be empty")
		}
		if _, exists := accounts[name]; exists {
			return nil, fmt.Errorf("duplicate OpenAI WS context policy account %q", rawName)
		}
		accounts[name] = struct{}{}
	}
	return &openAIWSContextPolicySnapshot{accounts: accounts}, nil
}
