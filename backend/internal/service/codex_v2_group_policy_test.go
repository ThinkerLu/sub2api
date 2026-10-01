package service

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const codexV2PolicyTestFixture = `
version: 1
groups:
  "12":
    enabled: true
  "34":
    enabled: false
`

func writeCodexV2PolicyFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func TestParseCodexV2GroupPolicySnapshotStrict(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
		wantErr string
	}{
		{
			name:    "valid",
			content: codexV2PolicyTestFixture,
		},
		{
			name:    "unknown_top_level_field",
			content: "version: 1\ngroup:\n  \"12\":\n    enabled: true\n",
			wantErr: "field group not found",
		},
		{
			name:    "unknown_group_field",
			content: "version: 1\ngroups:\n  \"12\":\n    enabled: true\n    parent: true\n",
			wantErr: "field parent not found",
		},
		{
			name:    "missing_enabled",
			content: "version: 1\ngroups:\n  \"12\": {}\n",
			wantErr: "must set enabled explicitly",
		},
		{
			name:    "group_id_not_integer",
			content: "version: 1\ngroups:\n  marketing:\n    enabled: true\n",
			wantErr: "must be a positive integer",
		},
		{
			name:    "group_id_zero",
			content: "version: 1\ngroups:\n  \"0\":\n    enabled: true\n",
			wantErr: "must be a positive integer",
		},
		{
			name:    "version_mismatch",
			content: "version: 2\ngroups:\n  \"12\":\n    enabled: true\n",
			wantErr: "version must be 1",
		},
		{
			name:    "second_document",
			content: "version: 1\ngroups:\n  \"12\":\n    enabled: true\n---\nversion: 1\ngroups: {}\n",
			wantErr: "multiple YAML documents",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			snapshot, err := parseCodexV2GroupPolicySnapshot([]byte(tt.content))
			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, 2, snapshot.groupCount)
			require.Equal(t, 1, snapshot.enabledCount)
			_, enabled := snapshot.enabledGroups[12]
			require.True(t, enabled)
			_, disabledExplicitly := snapshot.enabledGroups[34]
			require.False(t, disabledExplicitly)
		})
	}
}

func TestCodexV2GroupPolicyEnabledForGroupID(t *testing.T) {
	t.Parallel()

	var nilPolicy *CodexV2GroupPolicy
	require.False(t, nilPolicy.EnabledForGroupID(12), "nil policy must be disabled")

	unconfigured := &CodexV2GroupPolicy{}
	require.False(t, unconfigured.EnabledForGroupID(12), "unconfigured policy must be disabled")

	policy, err := NewCodexV2GroupPolicyFromBytes([]byte(codexV2PolicyTestFixture))
	require.NoError(t, err)
	require.True(t, policy.EnabledForGroupID(12))
	require.False(t, policy.EnabledForGroupID(34), "explicit enabled=false must stay disabled")
	require.False(t, policy.EnabledForGroupID(56), "unknown group must be disabled")
	require.False(t, policy.EnabledForGroupID(0))
	require.Equal(t, []int64{12}, policy.EnabledGroupIDs())
}

func TestInitializeCodexV2GroupPolicyRequiresValidFileWhenConfigured(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	disabled, err := InitializeCodexV2GroupPolicy(&config.Config{})
	require.NoError(t, err)
	require.False(t, disabled.EnabledForGroupID(12), "no configured file means the whole feature is off")
	require.False(t, disabled.SnapshotInfo()["configured"].(bool))
	t.Cleanup(disabled.Close)

	path := writeCodexV2PolicyFile(t, dir, "policy.yaml", codexV2PolicyTestFixture)
	enabled, err := InitializeCodexV2GroupPolicy(&config.Config{CodexV2GroupPolicy: config.CodexV2GroupPolicyConfig{File: path}})
	require.NoError(t, err)
	require.True(t, enabled.EnabledForGroupID(12))
	t.Cleanup(enabled.Close)

	_, err = InitializeCodexV2GroupPolicy(&config.Config{CodexV2GroupPolicy: config.CodexV2GroupPolicyConfig{File: filepath.Join(dir, "missing.yaml")}})
	require.Error(t, err, "configured-but-missing policy file must fail startup")

	invalidPath := writeCodexV2PolicyFile(t, dir, "invalid.yaml", "version: 1\ngroups:\n  \"12\": {}\n")
	_, err = InitializeCodexV2GroupPolicy(&config.Config{CodexV2GroupPolicy: config.CodexV2GroupPolicyConfig{File: invalidPath}})
	require.Error(t, err, "configured-but-invalid policy file must fail startup")
}

func TestCodexV2GroupPolicyReloadKeepsLastValidSnapshot(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := writeCodexV2PolicyFile(t, dir, "policy.yaml", codexV2PolicyTestFixture)
	policy := &CodexV2GroupPolicy{configured: true, path: path, stopCh: make(chan struct{}), doneCh: make(chan struct{})}
	require.NoError(t, policy.Reload())
	require.True(t, policy.EnabledForGroupID(12))

	require.NoError(t, os.WriteFile(path, []byte("version: 1\ngroups:\n  \"12\": {}\n"), 0o600))
	require.Error(t, policy.Reload(), "invalid reload must report an error")
	require.True(t, policy.EnabledForGroupID(12), "invalid reload must keep the previous snapshot")

	require.NoError(t, os.WriteFile(path, []byte("version: 1\ngroups:\n  \"12\":\n    enabled: false\n  \"34\":\n    enabled: true\n"), 0o600))
	require.NoError(t, policy.Reload())
	require.False(t, policy.EnabledForGroupID(12))
	require.True(t, policy.EnabledForGroupID(34))

	require.NoError(t, os.Remove(path))
	require.Error(t, policy.Reload(), "removal must report an error")
	require.True(t, policy.EnabledForGroupID(34), "removal must keep the previous snapshot")
}

func TestCodexV2GroupPolicyPollerPicksUpChanges(t *testing.T) {
	dir := t.TempDir()
	path := writeCodexV2PolicyFile(t, dir, "policy.yaml", codexV2PolicyTestFixture)
	policy := &CodexV2GroupPolicy{
		configured:     true,
		path:           path,
		reloadInterval: 20 * time.Millisecond,
		stopCh:         make(chan struct{}),
		doneCh:         make(chan struct{}),
	}
	require.NoError(t, policy.Reload())
	policy.Start()
	t.Cleanup(policy.Close)

	require.NoError(t, os.WriteFile(path, []byte("version: 1\ngroups:\n  \"12\":\n    enabled: false\n"), 0o600))
	require.Eventually(t, func() bool {
		return !policy.EnabledForGroupID(12)
	}, 3*time.Second, 20*time.Millisecond, "poller must apply the new snapshot")
}

// 并发读 + 反复重载必须只看到完整快照：要么启用要么关闭，不能 panic 或读到半成品。
func TestCodexV2GroupPolicyReloadIsAtomicUnderConcurrentReads(t *testing.T) {
	dir := t.TempDir()
	path := writeCodexV2PolicyFile(t, dir, "policy.yaml", codexV2PolicyTestFixture)
	policy := &CodexV2GroupPolicy{configured: true, path: path, stopCh: make(chan struct{}), doneCh: make(chan struct{})}
	require.NoError(t, policy.Reload())

	done := make(chan struct{})
	var readers sync.WaitGroup
	for i := 0; i < 8; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-done:
					return
				default:
					_ = policy.EnabledForGroupID(12)
					_ = policy.SnapshotInfo()
				}
			}
		}()
	}

	enabledFile := []byte("version: 1\ngroups:\n  \"12\":\n    enabled: true\n")
	disabledFile := []byte("version: 1\ngroups:\n  \"12\":\n    enabled: false\n")
	for i := 0; i < 60; i++ {
		content := enabledFile
		if i%2 == 0 {
			content = disabledFile
		}
		require.NoError(t, os.WriteFile(path, content, 0o600))
		require.NoError(t, policy.Reload())
	}
	close(done)
	readers.Wait()
}

func TestCodexV2PolicyDecisionPinnedPerRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)

	policy, err := NewCodexV2GroupPolicyFromBytes([]byte(codexV2PolicyTestFixture))
	require.NoError(t, err)
	restore := SetCurrentCodexV2GroupPolicyForTest(policy)
	t.Cleanup(restore)

	groupID := int64(12)
	newContext := func() *gin.Context {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Set("api_key", &APIKey{ID: 7, GroupID: &groupID})
		return c
	}

	c := newContext()
	require.True(t, codexV2PolicyEnabledForRequest(c))

	// 热重载把该分组关掉后，同一请求仍使用首次固定的快照（避免请求内行为漂移）。
	disabledPolicy, err := NewCodexV2GroupPolicyFromBytes([]byte("version: 1\ngroups:\n  \"12\":\n    enabled: false\n"))
	require.NoError(t, err)
	SetCurrentCodexV2GroupPolicyForTest(disabledPolicy)
	require.True(t, codexV2PolicyEnabledForRequest(c), "already pinned decision must not change mid-request")

	require.False(t, codexV2PolicyEnabledForRequest(newContext()), "new requests must observe the new snapshot")
}

func TestCodexV2PolicyDecisionWithoutAPIKey(t *testing.T) {
	gin.SetMode(gin.TestMode)

	policy, err := NewCodexV2GroupPolicyFromBytes([]byte(codexV2PolicyTestFixture))
	require.NoError(t, err)
	restore := SetCurrentCodexV2GroupPolicyForTest(policy)
	t.Cleanup(restore)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	require.False(t, codexV2PolicyEnabledForRequest(c), "requests without an API key group must stay disabled")

	plainGroup := &APIKey{ID: 8}
	c2, _ := gin.CreateTestContext(httptest.NewRecorder())
	c2.Set("api_key", plainGroup)
	require.False(t, codexV2PolicyEnabledForRequest(c2), "API keys without a group must stay disabled")
}

func TestCheckCodexV2GroupPolicyFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := writeCodexV2PolicyFile(t, dir, "policy.yaml", codexV2PolicyTestFixture)
	result, err := CheckCodexV2GroupPolicyFile(path)
	require.NoError(t, err)
	require.Equal(t, codexV2GroupPolicyVersion, result.Version)
	require.Equal(t, 2, result.GroupCount)
	require.Equal(t, []int64{12}, result.EnabledGroups)
	require.Len(t, result.SourceSHA256, 64)

	_, err = CheckCodexV2GroupPolicyFile("")
	require.Error(t, err)
	_, err = CheckCodexV2GroupPolicyFile(filepath.Join(dir, "missing.yaml"))
	require.Error(t, err)

	invalidPath := writeCodexV2PolicyFile(t, dir, "invalid.yaml", "version: 1\ngroups:\n  \"12\":\n    enabled: yes-please\n")
	_, err = CheckCodexV2GroupPolicyFile(invalidPath)
	require.Error(t, err, "a non-boolean enabled value must be rejected")
}
