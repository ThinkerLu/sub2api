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
	path     string
	snapshot atomic.Pointer[openAIWSContextPolicySnapshot]
	reloadMu sync.Mutex
}

var processOpenAIWSContextPolicy atomic.Pointer[OpenAIWSContextPolicy]

// CurrentOpenAIWSContextPolicy returns the process-level policy. A nil result
// means the feature has not been initialized, which preserves legacy behavior.
func CurrentOpenAIWSContextPolicy() *OpenAIWSContextPolicy {
	return processOpenAIWSContextPolicy.Load()
}

// InitializeOpenAIWSContextPolicy loads and publishes the process-level policy.
// A missing file disables the policy. An existing but invalid file fails
// startup instead of silently applying a partial or unintended account scope.
func InitializeOpenAIWSContextPolicy(cfg *config.Config) (*OpenAIWSContextPolicy, error) {
	policy := &OpenAIWSContextPolicy{}
	if cfg != nil {
		policy.path = strings.TrimSpace(cfg.Gateway.OpenAIWS.ContextPolicy.File)
	}
	if policy.path != "" {
		if err := policy.Reload(); err != nil {
			return nil, fmt.Errorf("load OpenAI WS context policy: %w", err)
		}
	}
	processOpenAIWSContextPolicy.Store(policy)
	return policy, nil
}

func (p *OpenAIWSContextPolicy) Name() string { return "OpenAI WS context policy" }

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
	policy := &OpenAIWSContextPolicy{}
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
	if p == nil {
		return false
	}
	snapshot := p.snapshot.Load()
	if snapshot == nil || len(snapshot.accounts) == 0 {
		return false
	}
	if _, ok := snapshot.accounts[strings.TrimSpace(accountName)]; !ok {
		return false
	}
	return true
}

func (p *OpenAIWSContextPolicy) Reload() error {
	if p == nil || strings.TrimSpace(p.path) == "" {
		return nil
	}
	p.reloadMu.Lock()
	defer p.reloadMu.Unlock()

	info, err := os.Stat(p.path)
	if errors.Is(err, os.ErrNotExist) {
		p.snapshot.Store(nil)
		log.Printf("[OpenAIWSContextPolicy] disabled; policy file %s is absent", p.path)
		return nil
	}
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
		return nil
	}
	snapshot, err := parseOpenAIWSContextPolicySnapshot(data)
	if err != nil {
		return err
	}
	snapshot.sourceSHA256 = hash
	snapshot.loadedAt = time.Now().UTC()
	p.snapshot.Store(snapshot)
	log.Printf("[OpenAIWSContextPolicy] loaded %d scoped accounts from %s", len(snapshot.accounts), p.path)
	return nil
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
