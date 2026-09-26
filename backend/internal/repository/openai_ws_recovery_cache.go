package repository

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

var _ service.OpenAIWSRecoveryCache = (*gatewayCache)(nil)

func openAIWSRecoveryCacheKey(kind, key string) (string, error) {
	if (kind != "owner" && kind != "migration" && kind != "replay") || len(key) != 64 {
		return "", service.ErrOpenAIWSRecoveryRejected
	}
	return fmt.Sprintf("openai:ws-recovery:v1:%s:%s", kind, key), nil
}

func (c *gatewayCache) CreateOpenAIWSRecoveryState(ctx context.Context, kind, key string, payload []byte, ttl time.Duration) (bool, error) {
	if c == nil || c.rdb == nil {
		return false, service.ErrOpenAIWSRecoveryUnavailable
	}
	if len(payload) == 0 || len(payload) > 16*1024 || ttl <= 0 {
		return false, service.ErrOpenAIWSRecoveryRejected
	}
	k, err := openAIWSRecoveryCacheKey(kind, key)
	if err != nil {
		return false, err
	}
	return c.rdb.SetNX(ctx, k, payload, ttl).Result()
}

func (c *gatewayCache) GetOpenAIWSRecoveryState(ctx context.Context, kind, key string) ([]byte, error) {
	if c == nil || c.rdb == nil {
		return nil, service.ErrOpenAIWSRecoveryUnavailable
	}
	k, err := openAIWSRecoveryCacheKey(kind, key)
	if err != nil {
		return nil, err
	}
	payload, err := c.rdb.Get(ctx, k).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	return payload, err
}

var swapOpenAIWSRecoveryStateScript = redis.NewScript(`
local current=redis.call('GET',KEYS[1])
if current==false or current~=ARGV[1] then return 0 end
local ttl=redis.call('PTTL',KEYS[1])
if ttl<=0 then return 0 end
if tonumber(ARGV[3])>0 then ttl=tonumber(ARGV[3]) end
redis.call('SET',KEYS[1],ARGV[2],'PX',ttl)
return 1
`)

func (c *gatewayCache) CompareAndSwapOpenAIWSRecoveryState(ctx context.Context, kind, key string, expected, replacement []byte, replaceTTL ...time.Duration) (bool, error) {
	if c == nil || c.rdb == nil {
		return false, service.ErrOpenAIWSRecoveryUnavailable
	}
	ownerConflict := kind == "owner" && bytes.Equal(replacement, append([]byte("ambiguous:"), expected...)) && len(replaceTTL) == 0
	if (kind != "migration" && !ownerConflict) || len(expected) == 0 || len(replacement) == 0 || len(replacement) > 32*1024 {
		return false, service.ErrOpenAIWSRecoveryRejected
	}
	k, err := openAIWSRecoveryCacheKey(kind, key)
	if err != nil {
		return false, err
	}
	ttl := int64(0)
	if len(replaceTTL) > 0 {
		if replaceTTL[0] <= 0 || replaceTTL[0] > 2*time.Minute {
			return false, service.ErrOpenAIWSRecoveryRejected
		}
		ttl = replaceTTL[0].Milliseconds()
	}
	n, err := swapOpenAIWSRecoveryStateScript.Run(ctx, c.rdb, []string{k}, expected, replacement, ttl).Int()
	return n == 1, err
}
