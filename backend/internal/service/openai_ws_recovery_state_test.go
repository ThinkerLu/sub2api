package service

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type wsRecoveryCacheStub struct {
	stubGatewayCache
	mu     sync.Mutex
	states map[string][]byte
}

func (c *wsRecoveryCacheStub) CreateOpenAIWSRecoveryState(_ context.Context, kind, key string, value []byte, _ time.Duration) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := kind + key
	if _, ok := c.states[k]; ok {
		return false, nil
	}
	c.states[k] = append([]byte(nil), value...)
	return true, nil
}
func (c *wsRecoveryCacheStub) GetOpenAIWSRecoveryState(_ context.Context, kind, key string) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.states[kind+key]...), nil
}
func (c *wsRecoveryCacheStub) CompareAndSwapOpenAIWSRecoveryState(_ context.Context, kind, key string, expected, replacement []byte, _ ...time.Duration) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := kind + key
	if string(c.states[k]) != string(expected) {
		return false, nil
	}
	c.states[k] = append([]byte(nil), replacement...)
	return true, nil
}
func wsRecoveryFixture() (*OpenAIGatewayService, OpenAIWSResponseOwner) {
	return &OpenAIGatewayService{cache: &wsRecoveryCacheStub{states: make(map[string][]byte)}}, OpenAIWSResponseOwner{Version: 1, GroupID: 1, UserID: 2, APIKeyID: 3, AccountID: 4, ResponseID: "resp_test", RequestedModel: "old", UpstreamModel: "gpt-5.1", Scope: "scope", Terminal: true}
}
func TestWSRecoveryOwnerRequiresExactUserKeyAndGroup(t *testing.T) {
	s, owner := wsRecoveryFixture()
	ctx := context.Background()
	require.NoError(t, s.SaveOpenAIWSResponseOwner(ctx, owner))
	require.NoError(t, s.SaveOpenAIWSResponseOwner(ctx, owner))
	for _, ids := range [][3]int64{{1, 2, 9}, {1, 9, 3}, {9, 2, 3}} {
		_, err := s.LoadOpenAIWSResponseOwner(ctx, ids[0], owner.ResponseID, ids[1], ids[2])
		require.ErrorIs(t, err, ErrOpenAIWSRecoveryRejected)
	}
	other := owner
	other.AccountID++
	require.ErrorIs(t, s.SaveOpenAIWSResponseOwner(ctx, other), ErrOpenAIWSRecoveryRejected)
	actual, err := s.LoadOpenAIWSResponseOwner(ctx, 1, owner.ResponseID, 2, 3)
	require.NoError(t, err)
	require.Equal(t, owner, *actual)
}
func TestWSMigrationIntentSingleClaimAndFailClosed(t *testing.T) {
	s, owner := wsRecoveryFixture()
	ctx := context.Background()
	intent := OpenAIWSMigrationIntent{Version: 1, Owner: owner, TargetAccountID: 5, RequestedModel: "new", UpstreamModel: "deepseek-v4-pro", InputSuffixDigest: strings.Repeat("a", 64), InputSuffixItems: 1, TurnID: "turn", NotSent: true, ExpiresAt: time.Now().Add(time.Minute).Unix()}
	require.NoError(t, s.CreateOpenAIWSMigrationIntent(ctx, intent))
	require.ErrorIs(t, s.CreateOpenAIWSMigrationIntent(ctx, intent), ErrOpenAIWSRecoveryRejected)
	loaded, raw, err := s.LoadOpenAIWSMigrationIntent(ctx, 1, 2, 3, "scope")
	require.NoError(t, err)
	require.Equal(t, intent, *loaded)
	wrong := intent
	wrong.TargetAccountID++
	require.ErrorIs(t, s.ClaimOpenAIWSMigrationIntent(ctx, wrong, raw), ErrOpenAIWSRecoveryRejected)
	require.NoError(t, s.ClaimOpenAIWSMigrationIntent(ctx, intent, raw))
	require.ErrorIs(t, s.ClaimOpenAIWSMigrationIntent(ctx, intent, raw), ErrOpenAIWSRecoveryRejected)
	_, _, err = s.LoadOpenAIWSMigrationIntent(ctx, 1, 2, 3, "scope")
	require.ErrorIs(t, err, ErrOpenAIWSRecoveryRejected)
	_, err = (&OpenAIGatewayService{cache: &stubGatewayCache{}}).LoadOpenAIWSResponseOwner(ctx, 1, "resp_old", 2, 3)
	require.ErrorIs(t, err, ErrOpenAIWSRecoveryUnavailable)
}
