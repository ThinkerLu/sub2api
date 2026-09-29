package service

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOpenAIWSContextPolicyAccountScope(t *testing.T) {
	policy, err := NewOpenAIWSContextPolicyFromBytes([]byte("version: 1\naccounts:\n  langbase: {}\n"))
	require.NoError(t, err)

	require.True(t, policy.AppliesTo("langbase"))
	require.False(t, policy.AppliesTo("other"))
	require.False(t, policy.ShouldRetainAdaptiveHistory("langbase", false))
	require.True(t, policy.ShouldRetainAdaptiveHistory("langbase", true))
	require.True(t, policy.ShouldRetainAdaptiveHistory("other", false))

	allAccounts, err := NewOpenAIWSContextPolicyFromBytes([]byte("version: 1\naccounts: {}\n"))
	require.NoError(t, err)
	require.True(t, allAccounts.AppliesTo("any-account"))
	require.False(t, allAccounts.ShouldRetainAdaptiveHistory("any-account", false))

	var nilPolicy *OpenAIWSContextPolicy
	require.True(t, nilPolicy.ShouldRetainAdaptiveHistory("langbase", false))
	disabled := &OpenAIWSContextPolicy{}
	require.False(t, disabled.AppliesTo("langbase"))
	require.True(t, disabled.ShouldRetainAdaptiveHistory("langbase", false))
}

func TestOpenAIWSContextPolicyRejectsInvalidDocuments(t *testing.T) {
	tests := map[string]string{
		"missing version":       "accounts: {}\n",
		"unsupported version":   "version: 2\naccounts: {}\n",
		"missing accounts":      "version: 1\n",
		"unknown root field":    "version: 1\naccounts: {}\nenabled: true\n",
		"unknown account field": "version: 1\naccounts:\n  langbase:\n    retain: false\n",
		"empty account name":    "version: 1\naccounts:\n  '  ': {}\n",
		"multiple documents":    "version: 1\naccounts: {}\n---\nversion: 1\naccounts: {}\n",
	}
	for name, document := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := NewOpenAIWSContextPolicyFromBytes([]byte(document))
			require.Error(t, err)
		})
	}
}

func TestOpenAIWSContextPolicyReloadKeepsLastValidSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ws-context-policy.yaml")
	require.NoError(t, os.WriteFile(path, []byte("version: 1\naccounts:\n  account-a: {}\n"), 0o600))
	policy := &OpenAIWSContextPolicy{
		enabled: true,
		path:    path,
		stopCh:  make(chan struct{}),
		doneCh:  make(chan struct{}),
	}
	require.NoError(t, policy.Reload())
	require.True(t, policy.AppliesTo("account-a"))

	require.NoError(t, os.WriteFile(path, []byte("version: 1\naccounts:\n  account-b:\n    unsupported: true\n"), 0o600))
	require.Error(t, policy.Reload())
	require.True(t, policy.AppliesTo("account-a"), "invalid reload must keep the previous valid scope")
	require.False(t, policy.AppliesTo("account-b"))

	require.NoError(t, os.WriteFile(path, []byte("version: 1\naccounts:\n  account-b: {}\n"), 0o600))
	require.NoError(t, policy.Reload())
	require.False(t, policy.AppliesTo("account-a"))
	require.True(t, policy.AppliesTo("account-b"))
}

func TestOpenAIWSContextPolicyDeploymentExample(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "deploy", "ws-context-policy.example.yaml"))
	require.NoError(t, err)
	policy, err := NewOpenAIWSContextPolicyFromBytes(data)
	require.NoError(t, err)
	require.True(t, policy.AppliesTo("account-a"))
	require.False(t, policy.AppliesTo("any-account"))
}

func TestAdaptiveContextPolicyPureWSUsesFullReconnect(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s, account, ws, httpUp := adaptiveFixture()
	account.Name = "context-policy-ws"
	policy, err := NewOpenAIWSContextPolicyFromBytes([]byte("version: 1\naccounts:\n  context-policy-ws: {}\n"))
	require.NoError(t, err)
	restore := SetCurrentOpenAIWSContextPolicyForTest(policy)
	t.Cleanup(restore)

	server, errs := startAdaptiveServer(t, s, account, nil)
	client := dialAdaptive(t, server)
	writeAdaptive(t, client, `{"type":"response.create","model":"gpt-5.1","input":"first"}`)
	sent := requirePassthroughUpstreamWrite(t, ws, 3*time.Second)
	require.Equal(t, "first", gjson.GetBytes(sent, "input").String())
	ws.Send(`{"type":"response.completed","response":{"id":"resp_ws_policy_1","model":"gpt-5.1","usage":{"input_tokens":1,"output_tokens":1}}}`)
	readAdaptiveCompleted(t, client)

	writeAdaptive(t, client, `{"type":"response.create","model":"gpt-5.1","previous_response_id":"resp_ws_policy_1","input":"continue"}`)
	select {
	case err := <-errs:
		require.ErrorContains(t, err, "cannot restore unknown previous_response_id")
		var closeErr *OpenAIWSClientCloseError
		require.True(t, errors.As(err, &closeErr))
		require.Equal(t, coderws.StatusPolicyViolation, closeErr.StatusCode())
	case <-time.After(3 * time.Second):
		t.Fatal("pure WS history was not discarded")
	}
	require.Empty(t, httpUp.snapshot())
}

func TestAdaptiveContextPolicyUnscopedAccountRetainsHistory(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s, account, ws, httpUp := adaptiveFixture()
	account.Name = "context-policy-other"
	policy, err := NewOpenAIWSContextPolicyFromBytes([]byte("version: 1\naccounts:\n  context-policy-selected: {}\n"))
	require.NoError(t, err)
	restore := SetCurrentOpenAIWSContextPolicyForTest(policy)
	t.Cleanup(restore)

	server, _ := startAdaptiveServer(t, s, account, nil)
	client := dialAdaptive(t, server)
	writeAdaptive(t, client, `{"type":"response.create","model":"gpt-5.1","input":"first"}`)
	requirePassthroughUpstreamWrite(t, ws, 3*time.Second)
	ws.Send(`{"type":"response.completed","response":{"id":"resp_ws_unscoped_1","model":"gpt-5.1","usage":{"input_tokens":1,"output_tokens":1}}}`)
	readAdaptiveCompleted(t, client)

	writeAdaptive(t, client, `{"type":"response.create","model":"gpt-5.1","previous_response_id":"resp_ws_unscoped_1","input":"continue"}`)
	sent := requirePassthroughUpstreamWrite(t, ws, 3*time.Second)
	require.Equal(t, "resp_ws_unscoped_1", gjson.GetBytes(sent, "previous_response_id").String())
	ws.Send(`{"type":"response.completed","response":{"id":"resp_ws_unscoped_2","model":"gpt-5.1","usage":{"input_tokens":1,"output_tokens":1}}}`)
	readAdaptiveCompleted(t, client)
	require.Empty(t, httpUp.snapshot())
}

func TestAdaptiveContextPolicyHTTPBridgeRetainsSelfContainedHistory(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s, account, _, httpUp := adaptiveFixture()
	account.Name = "context-policy-http"
	policy, err := NewOpenAIWSContextPolicyFromBytes([]byte("version: 1\naccounts:\n  context-policy-http: {}\n"))
	require.NoError(t, err)
	restore := SetCurrentOpenAIWSContextPolicyForTest(policy)
	t.Cleanup(restore)

	server, _ := startAdaptiveServer(t, s, account, nil)
	client := dialAdaptive(t, server)
	largeInput := strings.Repeat("x", 2048)
	writeAdaptive(t, client, `{"type":"response.create","model":"gpt-5.1","input":"`+largeInput+`"}`)
	readAdaptiveCompleted(t, client)
	writeAdaptive(t, client, `{"type":"response.create","model":"gpt-5.1","previous_response_id":"resp_http_1","input":"continue"}`)
	readAdaptiveCompleted(t, client)

	requests := httpUp.snapshot()
	require.Len(t, requests, 2)
	require.Contains(t, string(requests[1]), largeInput)
	require.Contains(t, string(requests[1]), "answer")
	require.False(t, gjson.GetBytes(requests[1], "previous_response_id").Exists())
}
