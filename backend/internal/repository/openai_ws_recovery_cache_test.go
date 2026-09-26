package repository

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestWSRecoveryCacheCrossInstanceClaimAndExpiry(t *testing.T) {
	mr := miniredis.RunT(t)
	client1 := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client1.Close()
	client2 := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client2.Close()
	caches := []service.OpenAIWSRecoveryCache{NewGatewayCache(client1).(service.OpenAIWSRecoveryCache), NewGatewayCache(client2).(service.OpenAIWSRecoveryCache)}
	ctx := context.Background()
	key := strings.Repeat("a", 64)
	created, err := caches[0].CreateOpenAIWSRecoveryState(ctx, "migration", key, []byte("pending"), time.Minute)
	require.NoError(t, err)
	require.True(t, created)
	var wg sync.WaitGroup
	var winners atomic.Int32
	for i := range 32 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ok, err := caches[i%2].CompareAndSwapOpenAIWSRecoveryState(ctx, "migration", key, []byte("pending"), []byte("consumed"))
			if err == nil && ok {
				winners.Add(1)
			}
		}(i)
	}
	wg.Wait()
	require.EqualValues(t, 1, winners.Load())
	payload, err := caches[1].GetOpenAIWSRecoveryState(ctx, "migration", key)
	require.NoError(t, err)
	require.Equal(t, "consumed", string(payload))
	created, err = caches[1].CreateOpenAIWSRecoveryState(ctx, "migration", key, []byte("replacement"), time.Minute)
	require.NoError(t, err)
	require.False(t, created)
	mr.FastForward(time.Minute)
	payload, err = caches[0].GetOpenAIWSRecoveryState(ctx, "migration", key)
	require.NoError(t, err)
	require.Empty(t, payload)
	ok, err := caches[1].CompareAndSwapOpenAIWSRecoveryState(ctx, "migration", key, []byte("consumed"), []byte("again"))
	require.NoError(t, err)
	require.False(t, ok)
}

func TestWSRecoveryOwnerCannotBeOverwritten(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()
	cache := NewGatewayCache(client).(service.OpenAIWSRecoveryCache)
	ctx := context.Background()
	key := strings.Repeat("b", 64)
	ok, err := cache.CreateOpenAIWSRecoveryState(ctx, "owner", key, []byte("account-one"), time.Minute)
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = cache.CreateOpenAIWSRecoveryState(ctx, "owner", key, []byte("account-two"), time.Minute)
	require.NoError(t, err)
	require.False(t, ok)
	_, err = cache.CompareAndSwapOpenAIWSRecoveryState(ctx, "owner", key, []byte("account-one"), []byte("account-two"))
	require.ErrorIs(t, err, service.ErrOpenAIWSRecoveryRejected)
	payload, err := cache.GetOpenAIWSRecoveryState(ctx, "owner", key)
	require.NoError(t, err)
	require.Equal(t, "account-one", string(payload))
}
