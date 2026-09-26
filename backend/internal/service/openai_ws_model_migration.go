package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

type OpenAIWSMigrationRetryError struct{ Event []byte }

func (*OpenAIWSMigrationRetryError) Error() string {
	return "model switch requires a full-context retry"
}

func OpenAIWSMigrationScope(c *gin.Context, body []byte, apiKeyID int64) string {
	scope, _ := resolveOpenAIWSExecutionScope(c, body, apiKeyID)
	return scope
}

func (s *OpenAIGatewayService) OpenAIWSModelSwitchAllowed(c *gin.Context, groupID int64) bool {
	if s == nil || s.cfg == nil || !s.cfg.Gateway.OpenAIWS.ModeRouterV2Enabled || groupID <= 0 || c == nil || c.Request == nil {
		return false
	}
	fields := strings.Fields(c.GetHeader("User-Agent"))
	if len(fields) == 0 {
		return false
	}
	version, ok := strings.CutPrefix(fields[0], "codex_cli_rs/")
	if !ok {
		return false
	}
	// Only the client family verified by the real reconnect probes may retry.
	return version == "0.155.0-alpha.16.4" || version == "0.155.0"
}

func (s *OpenAIGatewayService) PrepareOpenAIWSModelMigration(ctx context.Context, c *gin.Context, account *Account, body []byte, requestedModel string, userID, apiKeyID int64, groupID *int64, completedResponse ...string) error {
	if groupID == nil || !s.OpenAIWSModelSwitchAllowed(c, *groupID) || !s.IsOpenAIWSAdaptive(account) {
		return nil
	}
	targetPlatform := account.Platform
	routeModel := requestedModel
	if key := getAPIKeyFromContext(c); key != nil && key.Group != nil && key.Group.Platform == PlatformComposite {
		var err error
		ctx, routeModel, err = s.ResolveOpenAIWSRoute(ctx, key.Group, requestedModel)
		if err != nil {
			return err
		}
		platform, _ := ResolvedTargetPlatformFromContext(ctx)
		targetPlatform = NormalizeOpenAICompatiblePlatform(platform)
	}
	mapping, restricted := s.ResolveChannelMappingAndRestrict(ctx, groupID, routeModel)
	if restricted {
		return ErrOpenAIWSRecoveryRejected
	}
	if account.Platform == targetPlatform && (account.IsModelSupported(routeModel) || account.IsModelSupported(mapping.MappedModel)) {
		return nil
	}
	previous := strings.TrimSpace(gjson.GetBytes(body, "previous_response_id").String())
	fullFrame := previous == ""
	if fullFrame && len(completedResponse) > 0 {
		previous = completedResponse[0]
	}
	if previous == "" {
		return ErrOpenAIWSRecoveryRejected
	}
	owner, err := s.LoadOpenAIWSResponseOwner(ctx, *groupID, previous, userID, apiKeyID)
	if err != nil {
		return err
	}
	scope := OpenAIWSMigrationScope(c, body, apiKeyID)
	if scope == "" || owner.Scope != scope || owner.AccountID != account.ID || owner.RequestedModel == requestedModel {
		return ErrOpenAIWSRecoveryRejected
	}
	if commit := openAIWSCommitFromContext(ctx); commit != nil && !commit.notSent() {
		return ErrOpenAIWSRecoveryRejected
	}
	selection, _, err := s.SelectAccountWithSchedulerForCapability(ctx, groupID, "", "", mapping.MappedModel, map[int64]struct{}{account.ID: {}}, OpenAIUpstreamTransportResponsesWebsocketV2Ingress, OpenAIEndpointCapabilityResponses, false, false, false, targetPlatform)
	if err != nil {
		return err
	}
	if selection == nil || selection.Account == nil {
		return ErrOpenAIWSRecoveryRejected
	}
	if selection.ReleaseFunc != nil {
		defer selection.ReleaseFunc()
	}
	target := selection.Account
	if target.ID == account.ID || !s.IsOpenAIWSAdaptive(target) {
		return ErrOpenAIWSRecoveryRejected
	}
	digest, count, err := openAIWSInputSuffixDigest(body, 0)
	if err != nil {
		return err
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return err
	}
	intent := OpenAIWSMigrationIntent{Version: 1, Owner: *owner, TargetAccountID: target.ID, RequestedModel: requestedModel, UpstreamModel: normalizeOpenAIModelForUpstream(target, target.GetMappedModel(mapping.MappedModel)), InputSuffixDigest: digest, InputSuffixItems: count, TurnID: hex.EncodeToString(id[:]), NotSent: true, ExpiresAt: time.Now().Add(90 * time.Second).Unix()}
	intent.TargetPlatform = target.Platform
	if openAIWSHistoryProtocol(intent.UpstreamModel) == "" {
		return ErrOpenAIWSRecoveryRejected
	}
	if fullFrame {
		if _, err := sanitizeOpenAIWSMigrationHistory(body, owner.UpstreamModel, intent.UpstreamModel); err != nil {
			return err
		}
	}
	if err := s.CreateOpenAIWSMigrationIntent(ctx, intent); err != nil {
		return err
	}
	if fullFrame {
		return &OpenAIWSMigrationRetryError{}
	}
	event := map[string]any{"type": "error", "status": 400, "error": map[string]any{"type": "invalid_request_error", "code": "previous_response_not_found", "message": "Previous response not found.", "param": "previous_response_id"}}
	if stream := gjson.GetBytes(body, "stream_id"); stream.Exists() {
		event["stream_id"] = json.RawMessage(stream.Raw)
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		return err
	}
	return &OpenAIWSMigrationRetryError{Event: encoded}
}

func (s *OpenAIGatewayService) ResumeOpenAIWSModelMigration(ctx context.Context, c *gin.Context, body []byte, requestedModel string, userID, apiKeyID int64, groupID *int64) (*OpenAIWSMigrationIntent, []byte, error) {
	if s == nil || s.cfg == nil || !s.cfg.Gateway.OpenAIWS.ModeRouterV2Enabled || groupID == nil {
		return nil, body, nil
	}
	scope := OpenAIWSMigrationScope(c, body, apiKeyID)
	if scope == "" {
		return nil, body, nil
	}
	cache, err := s.openAIWSRecoveryCache()
	if err != nil {
		return nil, nil, err
	}
	fullDigest, _, digestErr := openAIWSInputSuffixDigest(body, 0)
	replayKey := ""
	if digestErr == nil {
		replayKey = openAIWSRecoveryKey(*groupID, ":", userID, ":", apiKeyID, ":", scope, ":", requestedModel, ":", gjson.GetBytes(body, "generate").Type == gjson.False, ":", fullDigest)
		previous, err := cache.GetOpenAIWSRecoveryState(ctx, "replay", replayKey)
		if err != nil {
			return nil, nil, err
		}
		if len(previous) > 0 {
			return nil, nil, ErrOpenAIWSRecoveryRejected
		}
	}
	// Recovery reads remain active while the creation switch is off, so pending
	// retries cannot accidentally fall through to the old sticky account.
	intent, raw, err := s.LoadOpenAIWSMigrationIntent(ctx, *groupID, userID, apiKeyID, scope)
	if errors.Is(err, ErrOpenAIWSRecoveryRejected) && intent != nil && intent.Consumed {
		previousID := strings.TrimSpace(gjson.GetBytes(body, "previous_response_id").String())
		if previousID != "" && previousID != intent.Owner.ResponseID {
			owner, ownerErr := s.LoadOpenAIWSResponseOwner(ctx, *groupID, previousID, userID, apiKeyID)
			if ownerErr == nil && owner.Scope == scope && owner.AccountID == intent.TargetAccountID {
				return nil, body, nil
			}
		}
		if previousID == "" && digestErr == nil {
			return nil, body, nil
		}
	}
	if err != nil {
		return nil, nil, err
	}
	if intent == nil {
		return nil, body, nil
	}
	if !s.OpenAIWSModelSwitchAllowed(c, *groupID) || intent.RequestedModel != requestedModel || strings.TrimSpace(gjson.GetBytes(body, "previous_response_id").String()) != "" {
		return nil, nil, ErrOpenAIWSRecoveryRejected
	}
	owner, ownerErr := s.LoadOpenAIWSResponseOwner(ctx, *groupID, intent.Owner.ResponseID, userID, apiKeyID)
	if ownerErr != nil {
		return nil, nil, ownerErr
	}
	if *owner != intent.Owner {
		return nil, nil, ErrOpenAIWSRecoveryRejected
	}
	digest, _, err := openAIWSInputSuffixDigest(body, intent.InputSuffixItems)
	if err != nil || digest != intent.InputSuffixDigest {
		return nil, nil, ErrOpenAIWSRecoveryRejected
	}
	sanitized, err := sanitizeOpenAIWSMigrationHistory(body, intent.Owner.UpstreamModel, intent.UpstreamModel)
	if err != nil {
		return nil, nil, err
	}
	if err := s.ClaimOpenAIWSMigrationIntent(ctx, *intent, raw); err != nil {
		return nil, nil, err
	}
	created, err := cache.CreateOpenAIWSRecoveryState(ctx, "replay", replayKey, []byte(intent.TurnID), 2*time.Minute)
	if err != nil {
		return nil, nil, err
	}
	if !created {
		return nil, nil, ErrOpenAIWSRecoveryRejected
	}
	return intent, sanitized, nil
}

func (s *OpenAIGatewayService) SelectOpenAIWSMigrationTarget(ctx context.Context, groupID *int64, intent *OpenAIWSMigrationIntent, model, platform string) (*AccountSelectionResult, OpenAIAccountScheduleDecision, error) {
	if intent.TargetPlatform != "" && intent.TargetPlatform != platform {
		return nil, OpenAIAccountScheduleDecision{}, ErrOpenAIWSRecoveryRejected
	}
	accounts, err := s.listSchedulableAccounts(ctx, groupID, platform)
	if err != nil {
		return nil, OpenAIAccountScheduleDecision{}, err
	}
	excluded := make(map[int64]struct{}, len(accounts))
	found := false
	for _, account := range accounts {
		if account.ID == intent.TargetAccountID {
			found = true
		} else {
			excluded[account.ID] = struct{}{}
		}
	}
	if !found {
		return nil, OpenAIAccountScheduleDecision{}, ErrOpenAIWSRecoveryRejected
	}
	selection, decision, err := s.SelectAccountWithSchedulerForCapability(ctx, groupID, "", "", model, excluded, OpenAIUpstreamTransportResponsesWebsocketV2Ingress, OpenAIEndpointCapabilityResponses, false, false, false, platform)
	if err != nil {
		return nil, decision, err
	}
	if selection == nil || selection.Account == nil {
		return nil, decision, ErrOpenAIWSRecoveryRejected
	}
	account := selection.Account
	if account.ID != intent.TargetAccountID || !s.IsOpenAIWSAdaptive(account) || normalizeOpenAIModelForUpstream(account, account.GetMappedModel(model)) != intent.UpstreamModel {
		if selection.ReleaseFunc != nil {
			selection.ReleaseFunc()
		}
		return nil, decision, ErrOpenAIWSRecoveryRejected
	}
	return selection, decision, nil
}
