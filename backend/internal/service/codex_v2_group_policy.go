package service

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"gopkg.in/yaml.v3"
)

// codexV2GroupPolicyMaxFileBytes 限制策略文件大小，避免误挂载大文件反复解析。
const codexV2GroupPolicyMaxFileBytes int64 = 1 << 20

// codexV2GroupPolicyVersion 是当前唯一支持的策略文件版本。
const codexV2GroupPolicyVersion = 1

// codexV2GroupPolicyDocument 是严格解析的策略文件结构。未知字段一律拒绝，
// 避免拼错键名时静默变成“全关”并在生产里悄悄不生效。
type codexV2GroupPolicyDocument struct {
	Version int                                `yaml:"version"`
	Groups  map[string]codexV2GroupPolicyEntry `yaml:"groups"`
}

// codexV2GroupPolicyEntry 是单个 API Key 分组的开关。enabled 必须显式书写。
type codexV2GroupPolicyEntry struct {
	Enabled *bool `yaml:"enabled"`
}

// codexV2GroupPolicySnapshot 是不可变的策略快照。加载成功后整份替换，
// 运行期读取不加锁；解析失败时保留上一份有效快照。
type codexV2GroupPolicySnapshot struct {
	enabledGroups map[int64]struct{}
	groupCount    int
	enabledCount  int
	sourceSHA256  string
	loadedAt      time.Time
	path          string
}

// CodexV2GroupPolicy 持有按 API Key GroupID 生效的 Codex MultiAgentV2 策略。
// 与 ModelRegistry 相同，采用“文件 + 轮询 + SIGHUP + 原子快照”的部署形态。
type CodexV2GroupPolicy struct {
	configured     bool
	path           string
	reloadInterval time.Duration
	snapshot       atomic.Pointer[codexV2GroupPolicySnapshot]
	reloadMu       sync.Mutex
	startOnce      sync.Once
	stopOnce       sync.Once
	stopCh         chan struct{}
	doneCh         chan struct{}
	started        atomic.Bool
	lastErrorMu    sync.Mutex
	lastError      string
}

var processCodexV2GroupPolicy atomic.Pointer[CodexV2GroupPolicy]

// CurrentCodexV2GroupPolicy 返回进程级策略对象。nil 表示未配置策略文件，
// 此时所有分组都必须按“未启用”处理。
func CurrentCodexV2GroupPolicy() *CodexV2GroupPolicy {
	return processCodexV2GroupPolicy.Load()
}

// InitializeCodexV2GroupPolicy 加载并发布进程级策略。
//
// 语义与 ModelRegistry 不同：策略只由 codex_v2_group_policy.file 是否配置决定
// 开启。未配置路径时功能整体关闭且不启动轮询；配置了路径但启动时缺失、不可读、
// 超限或校验失败，则返回错误，调用方必须让进程启动失败，而不是退回全关。
func InitializeCodexV2GroupPolicy(cfg *config.Config) (*CodexV2GroupPolicy, error) {
	policy := &CodexV2GroupPolicy{stopCh: make(chan struct{}), doneCh: make(chan struct{})}
	if cfg != nil {
		path := strings.TrimSpace(cfg.CodexV2GroupPolicy.File)
		if path != "" {
			policy.configured = true
			policy.path = path
			policy.reloadInterval = time.Duration(cfg.CodexV2GroupPolicy.ReloadIntervalSeconds) * time.Second
			if err := policy.Reload(); err != nil {
				return nil, fmt.Errorf("load codex v2 group policy: %w", err)
			}
		}
	}
	processCodexV2GroupPolicy.Store(policy)
	policy.Start()
	return policy, nil
}

// SetCurrentCodexV2GroupPolicyForTest 替换进程级策略并返回恢复函数。
func SetCurrentCodexV2GroupPolicyForTest(policy *CodexV2GroupPolicy) func() {
	previous := processCodexV2GroupPolicy.Swap(policy)
	return func() { processCodexV2GroupPolicy.Store(previous) }
}

// NewCodexV2GroupPolicyFromBytes 解析内存中的策略文件，供预检与测试使用。
func NewCodexV2GroupPolicyFromBytes(data []byte) (*CodexV2GroupPolicy, error) {
	snapshot, err := parseCodexV2GroupPolicySnapshot(data)
	if err != nil {
		return nil, err
	}
	policy := &CodexV2GroupPolicy{stopCh: make(chan struct{}), doneCh: make(chan struct{})}
	policy.configured = true
	policy.snapshot.Store(snapshot)
	return policy, nil
}

func (p *CodexV2GroupPolicy) Start() {
	if p == nil || !p.configured || p.reloadInterval <= 0 {
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
		log.Printf("[CodexV2GroupPolicy] watching %s every %s", p.path, p.reloadInterval)
	})
}

func (p *CodexV2GroupPolicy) Close() {
	if p == nil || !p.started.Load() {
		return
	}
	p.stopOnce.Do(func() { close(p.stopCh) })
	<-p.doneCh
}

// Reload 读取并校验策略文件。失败时保留上一有效快照，仅返回错误。
func (p *CodexV2GroupPolicy) Reload() error {
	if p == nil || !p.configured {
		return nil
	}
	p.reloadMu.Lock()
	defer p.reloadMu.Unlock()

	info, err := os.Stat(p.path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", p.path, err)
	}
	if info.Size() > codexV2GroupPolicyMaxFileBytes {
		return fmt.Errorf("codex v2 group policy exceeds %d bytes", codexV2GroupPolicyMaxFileBytes)
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
	snapshot, err := parseCodexV2GroupPolicySnapshot(data)
	if err != nil {
		return err
	}
	snapshot.sourceSHA256 = hash
	snapshot.loadedAt = time.Now().UTC()
	snapshot.path = p.path
	p.snapshot.Store(snapshot)
	p.clearReloadError()
	log.Printf("[CodexV2GroupPolicy] loaded %d group policies (%d enabled) from %s",
		snapshot.groupCount, snapshot.enabledCount, p.path)
	return nil
}

// EnabledForGroupID 报告某个 API Key 分组是否启用一期跨模型兼容策略。
// 未配置策略文件、分组缺失或 enabled=false 都返回 false。
func (p *CodexV2GroupPolicy) EnabledForGroupID(groupID int64) bool {
	if p == nil || !p.configured || groupID <= 0 {
		return false
	}
	snapshot := p.snapshot.Load()
	if snapshot == nil {
		return false
	}
	_, ok := snapshot.enabledGroups[groupID]
	return ok
}

// EnabledGroupIDs 返回当前启用分组 ID 的副本，供只读预检与诊断使用。
func (p *CodexV2GroupPolicy) EnabledGroupIDs() []int64 {
	if p == nil {
		return nil
	}
	snapshot := p.snapshot.Load()
	if snapshot == nil {
		return nil
	}
	ids := make([]int64, 0, len(snapshot.enabledGroups))
	for id := range snapshot.enabledGroups {
		ids = append(ids, id)
	}
	for i := 1; i < len(ids); i++ {
		for j := i; j > 0 && ids[j] < ids[j-1]; j-- {
			ids[j], ids[j-1] = ids[j-1], ids[j]
		}
	}
	return ids
}

// SnapshotInfo 返回不含策略正文的诊断信息。
func (p *CodexV2GroupPolicy) SnapshotInfo() map[string]any {
	if p == nil {
		return map[string]any{"configured": false, "loaded": false}
	}
	snapshot := p.snapshot.Load()
	if snapshot == nil {
		return map[string]any{"configured": p.configured, "loaded": false}
	}
	return map[string]any{
		"configured":     p.configured,
		"loaded":         true,
		"groups":         snapshot.groupCount,
		"enabled_groups": snapshot.enabledCount,
		"source_sha256":  snapshot.sourceSHA256,
		"loaded_at":      snapshot.loadedAt,
	}
}

func (p *CodexV2GroupPolicy) logReloadError(err error) {
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
	log.Printf("[CodexV2GroupPolicy] reload failed; keeping previous snapshot: %v", err)
}

func (p *CodexV2GroupPolicy) clearReloadError() {
	p.lastErrorMu.Lock()
	p.lastError = ""
	p.lastErrorMu.Unlock()
}

func parseCodexV2GroupPolicySnapshot(data []byte) (*codexV2GroupPolicySnapshot, error) {
	var document codexV2GroupPolicyDocument
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("parse codex v2 group policy: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return nil, errors.New("parse codex v2 group policy: multiple YAML documents are not allowed")
	}
	if document.Version != codexV2GroupPolicyVersion {
		return nil, fmt.Errorf("codex v2 group policy version must be %d", codexV2GroupPolicyVersion)
	}
	snapshot := &codexV2GroupPolicySnapshot{
		enabledGroups: make(map[int64]struct{}, len(document.Groups)),
	}
	for rawGroupID, entry := range document.Groups {
		groupID, err := parseCodexV2GroupPolicyGroupID(rawGroupID)
		if err != nil {
			return nil, err
		}
		if entry.Enabled == nil {
			return nil, fmt.Errorf("codex v2 group policy group %q must set enabled explicitly", rawGroupID)
		}
		if _, duplicate := snapshot.enabledGroups[groupID]; duplicate {
			return nil, fmt.Errorf("codex v2 group policy group %d is declared more than once", groupID)
		}
		snapshot.groupCount++
		if *entry.Enabled {
			snapshot.enabledGroups[groupID] = struct{}{}
			snapshot.enabledCount++
		}
	}
	return snapshot, nil
}

func parseCodexV2GroupPolicyGroupID(raw string) (int64, error) {
	trimmed := strings.TrimSpace(raw)
	groupID, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil || groupID <= 0 {
		return 0, fmt.Errorf("codex v2 group policy group %q must be a positive integer API key group id", raw)
	}
	return groupID, nil
}

// CodexV2GroupPolicyCheckResult 是 --check-* 预检的脱敏结果。
type CodexV2GroupPolicyCheckResult struct {
	Path          string  `json:"path"`
	Version       int     `json:"version"`
	GroupCount    int     `json:"group_count"`
	EnabledGroups []int64 `json:"enabled_groups"`
	SourceSHA256  string  `json:"source_sha256"`
	SizeBytes     int     `json:"size_bytes"`
}

// CheckCodexV2GroupPolicyFile 只读校验一个策略文件，供部署前 --check-* 预检使用。
// 返回结果只包含分组 ID 与摘要，不包含策略正文。
func CheckCodexV2GroupPolicyFile(path string) (*CodexV2GroupPolicyCheckResult, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return nil, errors.New("codex v2 group policy file path is required")
	}
	info, err := os.Stat(trimmed)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", trimmed, err)
	}
	if info.Size() > codexV2GroupPolicyMaxFileBytes {
		return nil, fmt.Errorf("codex v2 group policy exceeds %d bytes", codexV2GroupPolicyMaxFileBytes)
	}
	data, err := os.ReadFile(trimmed)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", trimmed, err)
	}
	snapshot, err := parseCodexV2GroupPolicySnapshot(data)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(data)
	policy := &CodexV2GroupPolicy{}
	policy.snapshot.Store(snapshot)
	return &CodexV2GroupPolicyCheckResult{
		Path:          trimmed,
		Version:       codexV2GroupPolicyVersion,
		GroupCount:    snapshot.groupCount,
		EnabledGroups: policy.EnabledGroupIDs(),
		SourceSHA256:  hex.EncodeToString(digest[:]),
		SizeBytes:     len(data),
	}, nil
}
