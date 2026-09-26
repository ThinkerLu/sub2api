package service

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestWSMigrationDefaultClientCompatibilityWithoutFeatureFlags(t *testing.T) {
	s, _, _, _ := adaptiveFixture()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/responses", nil)
	for _, ua := range []string{"codex_cli_rs/0.155.0", "codex_cli_rs/0.155.0-alpha.16.4"} {
		c.Request.Header.Set("User-Agent", ua)
		require.True(t, s.OpenAIWSModelSwitchAllowed(c, 1234))
	}
	for _, ua := range []string{"", "unknown/0.155.0", "codex_cli_rs/0.1.0", "other codex_cli_rs/0.155.0"} {
		c.Request.Header.Set("User-Agent", ua)
		require.False(t, s.OpenAIWSModelSwitchAllowed(c, 1234))
	}
}

func TestWSMigrationHistoryRejectsMissingToolsAndHostedState(t *testing.T) {
	for _, input := range []string{
		`[{"type":"function_call_output","call_id":"orphan","output":"done"}]`,
		`[{"type":"function_call","call_id":"unanswered","name":"x","arguments":"{}"}]`,
		`[{"type":"compaction","encrypted_content":"opaque"}]`,
		`[{"role":"user","content":[{"type":"input_file","file_id":"file_old"}]}]`,
		`[{"type":"item_reference","id":"msg_old"}]`,
	} {
		_, err := sanitizeOpenAIWSMigrationHistory([]byte(`{"input":`+input+`}`), "deepseek-v4-pro", "gpt-5.1")
		require.Error(t, err)
	}
}

func TestWSMigrationHistoryPreservesMessagesAndToolOrder(t *testing.T) {
	body := []byte(`{"model":"target","input":[{"role":"user","content":"hello"},{"type":"reasoning","encrypted_content":"gAAAAA-test"},{"type":"function_call","call_id":"c1","name":"test","arguments":"{}"},{"type":"function_call_output","call_id":"c1","output":"ok"}]}`)
	original := string(body)
	for _, target := range []string{"gpt-5.1", "deepseek-v4-pro"} {
		out, err := sanitizeOpenAIWSMigrationHistory(body, "gpt-5.1", target)
		require.NoError(t, err)
		require.Equal(t, "hello", gjson.GetBytes(out, "input.0.content").String())
		require.Equal(t, "function_call", gjson.GetBytes(out, "input.1.type").String())
		require.Equal(t, "function_call_output", gjson.GetBytes(out, "input.2.type").String())
		require.Equal(t, original, string(body))
		again, err := sanitizeOpenAIWSMigrationHistory(out, "gpt-5.1", target)
		require.NoError(t, err)
		require.Equal(t, string(out), string(again))
	}
}

func TestWSMigrationFullRetryClaimsOnceAndAllowsNextTurn(t *testing.T) {
	s, owner := wsRecoveryFixture()
	s.cfg = passthroughLifecycleConfig()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/responses", nil)
	c.Request.Header.Set("User-Agent", "codex_cli_rs/0.155.0-alpha.16.4")
	c.Request.Header.Set("thread-id", "test-thread")
	owner.Scope = OpenAIWSMigrationScope(c, nil, owner.APIKeyID)
	ctx := context.Background()
	require.NoError(t, s.SaveOpenAIWSResponseOwner(ctx, owner))
	delta := []byte(`{"input":[{"role":"user","content":"next"}]}`)
	digest, count, err := openAIWSInputSuffixDigest(delta, 0)
	require.NoError(t, err)
	intent := OpenAIWSMigrationIntent{Version: 1, Owner: owner, TargetAccountID: 5, RequestedModel: "new", UpstreamModel: "deepseek-v4-pro", InputSuffixDigest: digest, InputSuffixItems: count, TurnID: "migration-turn", NotSent: true, ExpiresAt: time.Now().Add(time.Minute).Unix()}
	require.NoError(t, s.CreateOpenAIWSMigrationIntent(ctx, intent))
	body := []byte(`{"model":"new","input":[{"role":"user","content":"old"},{"type":"reasoning","encrypted_content":"gAAAAA-old"},{"role":"user","content":"next"}]}`)
	claimed, out, err := s.ResumeOpenAIWSModelMigration(ctx, c, body, "new", 2, 3, &owner.GroupID)
	require.NoError(t, err)
	require.EqualValues(t, 5, claimed.TargetAccountID)
	require.NotContains(t, string(out), "gAAAAA-old")
	_, _, err = s.ResumeOpenAIWSModelMigration(ctx, c, body, "new", 2, 3, &owner.GroupID)
	require.ErrorIs(t, err, ErrOpenAIWSRecoveryRejected)
	next := []byte(`{"model":"new","input":[{"role":"user","content":"later independent turn"}]}`)
	claimed, _, err = s.ResumeOpenAIWSModelMigration(ctx, c, next, "new", 2, 3, &owner.GroupID)
	require.NoError(t, err)
	require.Nil(t, claimed)
	// A new switch may replace the consumed session slot; the original full
	// retry still has a separate replay tombstone and cannot execute again.
	back := intent
	back.Owner.AccountID = 5
	back.Owner.ResponseID = "resp_new"
	back.Owner.RequestedModel = "new"
	back.TargetAccountID = 4
	back.RequestedModel = "old"
	back.TurnID = "back"
	require.NoError(t, s.CreateOpenAIWSMigrationIntent(ctx, back))
	_, _, err = s.ResumeOpenAIWSModelMigration(ctx, c, body, "new", 2, 3, &owner.GroupID)
	require.ErrorIs(t, err, ErrOpenAIWSRecoveryRejected)
}
