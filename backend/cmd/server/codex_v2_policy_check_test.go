package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRunCodexV2GroupPolicyCheck 覆盖 --check-codex-v2-group-policy-file 的预检分支：
// 有效文件返回 nil，无效文件返回错误。显式路径分支不读取运行配置。
func TestRunCodexV2GroupPolicyCheck(t *testing.T) {
	dir := t.TempDir()
	valid := filepath.Join(dir, "valid.yaml")
	require.NoError(t, os.WriteFile(valid, []byte("version: 1\ngroups:\n  \"12\":\n    enabled: true\n"), 0o600))
	require.NoError(t, runCodexV2GroupPolicyCheck(valid))

	invalid := filepath.Join(dir, "invalid.yaml")
	require.NoError(t, os.WriteFile(invalid, []byte("version: 1\ngroups:\n  \"12\": {}\n"), 0o600))
	require.Error(t, runCodexV2GroupPolicyCheck(invalid))

	require.Error(t, runCodexV2GroupPolicyCheck(filepath.Join(dir, "missing.yaml")))
}
