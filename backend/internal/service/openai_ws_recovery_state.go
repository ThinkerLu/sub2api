package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var ErrOpenAIWSRecoveryUnavailable = errors.New("shared websocket recovery state unavailable")
var ErrOpenAIWSRecoveryRejected = errors.New("websocket recovery state rejected")

// Implementations must provide shared atomic create/CAS. A local fallback is
// deliberately excluded because a reconnect can land on a different instance.
type OpenAIWSRecoveryCache interface {
	CreateOpenAIWSRecoveryState(context.Context, string, string, []byte, time.Duration) (bool, error)
	GetOpenAIWSRecoveryState(context.Context, string, string) ([]byte, error)
	CompareAndSwapOpenAIWSRecoveryState(context.Context, string, string, []byte, []byte, ...time.Duration) (bool, error)
}

type OpenAIWSResponseOwner struct {
	Version        int    `json:"version"`
	GroupID        int64  `json:"group_id"`
	UserID         int64  `json:"user_id"`
	APIKeyID       int64  `json:"api_key_id"`
	AccountID      int64  `json:"account_id"`
	ResponseID     string `json:"response_id"`
	RequestedModel string `json:"requested_model"`
	UpstreamModel  string `json:"upstream_model"`
	Scope          string `json:"scope"`
	Terminal       bool   `json:"terminal"`
}

type OpenAIWSMigrationIntent struct {
	Version           int                   `json:"version"`
	Owner             OpenAIWSResponseOwner `json:"owner"`
	TargetAccountID   int64                 `json:"target_account_id"`
	TargetPlatform    string                `json:"target_platform,omitempty"`
	RequestedModel    string                `json:"requested_model"`
	UpstreamModel     string                `json:"upstream_model"`
	InputSuffixDigest string                `json:"input_suffix_digest"`
	InputSuffixItems  int                   `json:"input_suffix_items"`
	TurnID            string                `json:"turn_id"`
	NotSent           bool                  `json:"not_sent"`
	Consumed          bool                  `json:"consumed"`
	ExpiresAt         int64                 `json:"expires_at"`
}

func openAIWSRecoveryKey(parts ...any) string {
	hash := sha256.Sum256([]byte(fmt.Sprint(parts...)))
	return hex.EncodeToString(hash[:])
}

func (s *OpenAIGatewayService) openAIWSRecoveryCache() (OpenAIWSRecoveryCache, error) {
	if s == nil {
		return nil, ErrOpenAIWSRecoveryUnavailable
	}
	cache, ok := s.cache.(OpenAIWSRecoveryCache)
	if !ok || cache == nil {
		return nil, ErrOpenAIWSRecoveryUnavailable
	}
	return cache, nil
}

func validOpenAIWSResponseOwner(owner OpenAIWSResponseOwner) bool {
	return owner.Version == 1 && owner.GroupID >= 0 && owner.UserID > 0 && owner.APIKeyID > 0 && owner.AccountID > 0 && owner.Terminal && strings.TrimSpace(owner.ResponseID) != "" && len(owner.ResponseID) <= 512 && owner.RequestedModel != "" && owner.UpstreamModel != ""
}

func (s *OpenAIGatewayService) SaveOpenAIWSResponseOwner(ctx context.Context, owner OpenAIWSResponseOwner) error {
	if !validOpenAIWSResponseOwner(owner) {
		return ErrOpenAIWSRecoveryRejected
	}
	cache, err := s.openAIWSRecoveryCache()
	if err != nil {
		return err
	}
	payload, err := json.Marshal(owner)
	if err != nil {
		return err
	}
	key := openAIWSRecoveryKey(owner.GroupID, ":", owner.ResponseID)
	created, err := cache.CreateOpenAIWSRecoveryState(ctx, "owner", key, payload, s.openAIWSResponseStickyTTL())
	if err != nil || created {
		return err
	}
	existing, err := cache.GetOpenAIWSRecoveryState(ctx, "owner", key)
	if err != nil {
		return err
	}
	if string(existing) != string(payload) {
		if !strings.HasPrefix(string(existing), "ambiguous:") {
			_, markErr := cache.CompareAndSwapOpenAIWSRecoveryState(ctx, "owner", key, existing, append([]byte("ambiguous:"), existing...))
			if markErr != nil {
				return markErr
			}
		}
		return ErrOpenAIWSRecoveryRejected
	}
	return nil
}

func (s *OpenAIGatewayService) LoadOpenAIWSResponseOwner(ctx context.Context, groupID int64, responseID string, userID, apiKeyID int64) (*OpenAIWSResponseOwner, error) {
	cache, err := s.openAIWSRecoveryCache()
	if err != nil {
		return nil, err
	}
	payload, err := cache.GetOpenAIWSRecoveryState(ctx, "owner", openAIWSRecoveryKey(groupID, ":", responseID))
	if err != nil {
		return nil, err
	}
	var owner OpenAIWSResponseOwner
	if json.Unmarshal(payload, &owner) != nil || !validOpenAIWSResponseOwner(owner) || owner.GroupID != groupID || owner.ResponseID != responseID || owner.UserID != userID || owner.APIKeyID != apiKeyID {
		return nil, ErrOpenAIWSRecoveryRejected
	}
	return &owner, nil
}

func openAIWSMigrationKey(groupID, userID, apiKeyID int64, scope string) string {
	return openAIWSRecoveryKey(groupID, ":", userID, ":", apiKeyID, ":", scope)
}

func (s *OpenAIGatewayService) CreateOpenAIWSMigrationIntent(ctx context.Context, intent OpenAIWSMigrationIntent) error {
	if intent.Version != 1 || !validOpenAIWSResponseOwner(intent.Owner) || intent.Owner.Scope == "" || intent.TargetAccountID <= 0 || intent.TargetAccountID == intent.Owner.AccountID || intent.RequestedModel == "" || intent.RequestedModel == intent.Owner.RequestedModel || intent.UpstreamModel == "" || intent.TurnID == "" || !intent.NotSent || intent.Consumed || intent.InputSuffixItems <= 0 || len(intent.InputSuffixDigest) != 64 {
		return ErrOpenAIWSRecoveryRejected
	}
	ttl := time.Until(time.Unix(intent.ExpiresAt, 0))
	if ttl <= 0 || ttl > 2*time.Minute {
		return ErrOpenAIWSRecoveryRejected
	}
	cache, err := s.openAIWSRecoveryCache()
	if err != nil {
		return err
	}
	payload, err := json.Marshal(intent)
	if err != nil {
		return err
	}
	key := openAIWSMigrationKey(intent.Owner.GroupID, intent.Owner.UserID, intent.Owner.APIKeyID, intent.Owner.Scope)
	created, err := cache.CreateOpenAIWSRecoveryState(ctx, "migration", key, payload, ttl)
	if err != nil {
		return err
	}
	if !created {
		previousRaw, readErr := cache.GetOpenAIWSRecoveryState(ctx, "migration", key)
		if readErr != nil {
			return readErr
		}
		var previous OpenAIWSMigrationIntent
		if json.Unmarshal(previousRaw, &previous) != nil || !previous.Consumed || previous.Owner.Scope != intent.Owner.Scope || previous.Owner.UserID != intent.Owner.UserID || previous.Owner.APIKeyID != intent.Owner.APIKeyID || previous.Owner.GroupID != intent.Owner.GroupID || previous.TargetAccountID != intent.Owner.AccountID || previous.Owner.ResponseID == intent.Owner.ResponseID {
			return ErrOpenAIWSRecoveryRejected
		}
		replaced, swapErr := cache.CompareAndSwapOpenAIWSRecoveryState(ctx, "migration", key, previousRaw, payload, ttl)
		if swapErr != nil {
			return swapErr
		}
		if !replaced {
			return ErrOpenAIWSRecoveryRejected
		}
	}
	return nil
}

func (s *OpenAIGatewayService) LoadOpenAIWSMigrationIntent(ctx context.Context, groupID, userID, apiKeyID int64, scope string) (*OpenAIWSMigrationIntent, []byte, error) {
	if scope == "" {
		return nil, nil, nil
	}
	cache, err := s.openAIWSRecoveryCache()
	if err != nil {
		return nil, nil, err
	}
	payload, err := cache.GetOpenAIWSRecoveryState(ctx, "migration", openAIWSMigrationKey(groupID, userID, apiKeyID, scope))
	if err != nil || len(payload) == 0 {
		return nil, nil, err
	}
	var intent OpenAIWSMigrationIntent
	if json.Unmarshal(payload, &intent) != nil || intent.Version != 1 || !validOpenAIWSResponseOwner(intent.Owner) || intent.Owner.GroupID != groupID || intent.Owner.UserID != userID || intent.Owner.APIKeyID != apiKeyID || intent.Owner.Scope != scope || intent.ExpiresAt <= time.Now().Unix() || !intent.NotSent {
		return nil, nil, ErrOpenAIWSRecoveryRejected
	}
	if intent.Consumed {
		return &intent, payload, ErrOpenAIWSRecoveryRejected
	}
	return &intent, payload, nil
}

func (s *OpenAIGatewayService) ClaimOpenAIWSMigrationIntent(ctx context.Context, intent OpenAIWSMigrationIntent, expected []byte) error {
	if intent.Consumed || !intent.NotSent || intent.ExpiresAt <= time.Now().Unix() {
		return ErrOpenAIWSRecoveryRejected
	}
	var previous OpenAIWSMigrationIntent
	if json.Unmarshal(expected, &previous) != nil || previous != intent {
		return ErrOpenAIWSRecoveryRejected
	}
	cache, err := s.openAIWSRecoveryCache()
	if err != nil {
		return err
	}
	intent.Consumed = true
	payload, err := json.Marshal(intent)
	if err != nil {
		return err
	}
	claimed, err := cache.CompareAndSwapOpenAIWSRecoveryState(ctx, "migration", openAIWSMigrationKey(intent.Owner.GroupID, intent.Owner.UserID, intent.Owner.APIKeyID, intent.Owner.Scope), expected, payload)
	if err != nil {
		return err
	}
	if !claimed {
		return ErrOpenAIWSRecoveryRejected
	}
	return nil
}
