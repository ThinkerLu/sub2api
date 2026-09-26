package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// IsOpenAIWSAdaptive reports the selected account's effective ingress mode.
func (s *OpenAIGatewayService) IsOpenAIWSAdaptive(account *Account) bool {
	return s != nil && s.cfg != nil && s.cfg.Gateway.OpenAIWS.ModeRouterV2Enabled && account != nil && account.ResolveOpenAIResponsesWebSocketV2Mode(s.cfg.Gateway.OpenAIWS.IngressModeDefault) == OpenAIWSIngressModeAdaptive
}

// OpenAIWSAdaptiveRouteAvailable runs before the model-bearing first message.
// It checks the same group/platform account inventory as the scheduler, without
// selecting an account or acquiring an account concurrency slot.
func (s *OpenAIGatewayService) OpenAIWSAdaptiveRouteAvailable(ctx context.Context, groupID *int64) (bool, error) {
	if s == nil || s.cfg == nil || !s.cfg.Gateway.OpenAIWS.ModeRouterV2Enabled {
		return false, nil
	}
	accounts, err := s.listSchedulableAccounts(ctx, groupID, PlatformOpenAI)
	if err != nil {
		return false, err
	}
	for i := range accounts {
		if s.IsOpenAIWSAdaptive(&accounts[i]) && s.getOpenAIWSProtocolResolver().Resolve(&accounts[i]).Transport == OpenAIUpstreamTransportResponsesWebsocketV2 {
			return true, nil
		}
	}
	return false, nil
}

// OpenAIWSModelRouteAvailable reports whether at least one schedulable account
// can serve the model over the native Responses WebSocket transport. It is a
// capability preflight, not an availability check: busy or temporarily blocked
// WS accounts still count here so normal queueing and failover can proceed.
func (s *OpenAIGatewayService) OpenAIWSModelRouteAvailable(
	ctx context.Context,
	groupID *int64,
	model string,
	requiredCapability OpenAIEndpointCapability,
) (bool, error) {
	if s == nil || s.cfg == nil {
		return true, nil
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return true, nil
	}
	accounts, err := s.listSchedulableAccounts(ctx, groupID, PlatformOpenAI)
	if err != nil {
		return false, err
	}
	ctx = s.withOpenAIQuotaAutoPauseContext(ctx)
	scheduler := &defaultOpenAIAccountScheduler{service: s}
	for i := range accounts {
		account := &accounts[i]
		if !account.IsSchedulable() {
			continue
		}
		req := OpenAIAccountScheduleRequest{
			GroupID:            groupID,
			Platform:           PlatformOpenAI,
			RequestedModel:     model,
			RequiredTransport:  OpenAIUpstreamTransportResponsesWebsocketV2Ingress,
			RequiredCapability: requiredCapability,
		}
		// Reuse the scheduler's eligibility checks so quota-paused, runtime-blocked,
		// group-incompatible and unsupported accounts cannot make the handshake
		// preflight claim that a usable native WS route exists. Busy accounts remain
		// eligible here because concurrency/load is intentionally not part of this
		// capability check.
		if compatible, _ := scheduler.isAccountRequestCompatibleReason(ctx, account, req); !compatible {
			continue
		}
		if scheduler.isAccountTransportCompatible(account, req.RequiredTransport) {
			return true, nil
		}
	}
	return false, nil
}

// The adaptive session owns the warmup socket until HTTP is selected or the
// session ends. All access is sequential, after the previous relay has joined.
type openAIWSAdaptiveUpstream struct {
	conn    openAIWSClientConn
	status  int
	headers http.Header
}

func (u *openAIWSAdaptiveUpstream) close() {
	if u != nil && u.conn != nil {
		_ = u.conn.Close()
		u.conn = nil
		u.headers = nil
	}
}

// Returning a handoff unwinds the bridge loop before starting the long-lived
// relay, so its local payloads/closures cannot retain pre-compaction history.
type openAIWSAdaptiveHandoff struct {
	payload                  []byte
	originalModel            string
	requestedReasoningEffort *string
	turn                     int
	startedAt                time.Time
	history                  []json.RawMessage
}

func (*openAIWSAdaptiveHandoff) Error() string { return "adaptive websocket handoff" }

func (s *OpenAIGatewayService) ProxyResponsesWebSocketFromClient(ctx context.Context, c *gin.Context, clientConn *coderws.Conn, account *Account, token string, firstClientMessage []byte, hooks *OpenAIWSIngressHooks) (returnErr error) {
	if ctx == nil {
		ctx = context.Background()
	}
	reader, _ := ctx.Value(openAIWSAdaptiveReaderKey{}).(*openAIWSAdaptiveReader)
	if s.IsOpenAIWSAdaptive(account) && clientConn != nil {
		if s.cfg.Gateway.OpenAIWS.DualLegEnabled {
			cleanupMemory, err := s.BindOpenAIWSDualLegMemory(clientConn)
			if err != nil {
				return err
			}
			defer cleanupMemory()
			value, _ := openAIWSMemorySessions.Load(clientConn)
			memory := value.(*openAIWSMemorySession)
			if err := memory.bindAccount(account.ID); err != nil {
				return err
			}
			ctx = context.WithValue(ctx, openAIWSMemoryContextKey{}, memory)
		}
		copied := OpenAIWSIngressHooks{}
		if hooks != nil {
			copied = *hooks
		}
		copied.adaptiveUpstream = &openAIWSAdaptiveUpstream{}
		hooks = &copied
		defer copied.adaptiveUpstream.close()
		if s.cfg.Gateway.OpenAIWS.DualLegEnabled {
			clientConn.SetReadLimit(ResolveOpenAIWSClientReadLimitBytes(s.cfg))
		} else {
			clientConn.SetReadLimit(-1)
		}
		if reader == nil {
			var cleanup func()
			ctx, cleanup = s.BeginOpenAIWSAdaptiveSession(ctx, clientConn)
			defer cleanup()
			reader, _ = ctx.Value(openAIWSAdaptiveReaderKey{}).(*openAIWSAdaptiveReader)
		}
	}
	if reader != nil {
		copied := OpenAIWSIngressHooks{}
		if hooks != nil {
			copied = *hooks
		}
		copied.adaptiveReader = reader
		hooks = &copied
		defer func() {
			if cause := context.Cause(reader.ctx); cause != nil && !errors.Is(cause, context.Canceled) {
				returnErr = cause
			}
		}()
	}

	err := s.proxyResponsesWebSocketFromClient(ctx, c, clientConn, account, token, firstClientMessage, hooks)
	firstClientMessage = nil
	var handoff *openAIWSAdaptiveHandoff
	if !errors.As(err, &handoff) {
		return err
	}
	// HTTP state is not a native WS connection binding.
	c.Request.Header.Del(openAIWSTurnStateHeader)
	c.Set("openai_ws_http_bridge", false)
	nextHooks := adaptivePassthroughHooks(hooks, handoff)
	return s.proxyResponsesWebSocketV2Passthrough(ctx, c, clientConn, account, token, handoff.payload, nextHooks, s.getOpenAIWSProtocolResolver().Resolve(account))
}

func adaptivePassthroughHooks(hooks *OpenAIWSIngressHooks, handoff *openAIWSAdaptiveHandoff) *OpenAIWSIngressHooks {
	next := OpenAIWSIngressHooks{}
	if hooks != nil {
		next = *hooks
	}
	next.initialPayloadPrepared = true
	next.initialRequestedReasoningEffort = handoff.requestedReasoningEffort
	next.InitialRequestModel = handoff.originalModel
	next.InitialTurnStartedAt = handoff.startedAt
	offset := handoff.turn - 1
	if hooks != nil {
		if hooks.BeforeRequest != nil {
			next.BeforeRequest = func(t int, p []byte, m string) error { return hooks.BeforeRequest(t+offset, p, m) }
		}
		if hooks.BeforeTurn != nil {
			next.BeforeTurn = func(t int) error { return hooks.BeforeTurn(t + offset) }
		}
		if hooks.MapRequestModel != nil {
			next.MapRequestModel = func(t int, m string) (string, error) { return hooks.MapRequestModel(t+offset, m) }
		}
		if hooks.TurnStarted != nil {
			next.TurnStarted = func(t int, at time.Time) { hooks.TurnStarted(t+offset, at) }
		}
	}
	next.AfterTurn = func(t int, result *OpenAIForwardResult, turnErr error) {
		if t == 1 && turnErr == nil && result != nil && result.SucceededForScheduling() {
			handoff.history = nil
			handoff.payload = nil
		}
		if hooks != nil && hooks.AfterTurn != nil {
			hooks.AfterTurn(t+offset, result, turnErr)
		}
	}
	return &next
}

// One reader owns the downstream socket across both executors. A single pending
// message is enough: overlapping response.create is rejected instead of queued.
type openAIWSAdaptiveFrame struct {
	kind    coderws.MessageType
	payload []byte
}
type openAIWSAdaptiveReader struct {
	ctx       context.Context
	frames    chan openAIWSAdaptiveFrame
	active    atomic.Bool
	idleStart chan struct{}
}

func newOpenAIWSAdaptiveReader(ctx context.Context, cancel context.CancelCauseFunc, conn *coderws.Conn, idle time.Duration) *openAIWSAdaptiveReader {
	r := &openAIWSAdaptiveReader{ctx: ctx, frames: make(chan openAIWSAdaptiveFrame, 1), idleStart: make(chan struct{}, 1)}
	r.active.Store(true)
	go func() {
		for {
			kind, payload, err := readOpenAIWSClientMessageWithTimeoutStart(ctx, conn, idle, coderws.StatusNormalClosure, "websocket idle timeout", r.idleStart, func() bool { return !r.active.Load() })
			if err != nil {
				cancel(err)
				return
			}
			event := strings.TrimSpace(gjson.GetBytes(payload, "type").String())
			if (event == "response.create" || event == "") && !r.active.CompareAndSwap(false, true) {
				cancel(NewOpenAIWSClientCloseError(coderws.StatusPolicyViolation, "overlapping response.create is not supported", nil))
				return
			}
			select {
			case r.frames <- openAIWSAdaptiveFrame{kind, payload}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return r
}
func (r *openAIWSAdaptiveReader) read(ctx context.Context) (coderws.MessageType, []byte, error) {
	select {
	case <-r.ctx.Done():
		return coderws.MessageText, nil, context.Cause(r.ctx)
	default:
	}
	select {
	case f := <-r.frames:
		return f.kind, f.payload, nil
	case <-r.ctx.Done():
		return coderws.MessageText, nil, context.Cause(r.ctx)
	case <-ctx.Done():
		return coderws.MessageText, nil, ctx.Err()
	}
}
func (r *openAIWSAdaptiveReader) beforeWrite(payload []byte) {
	if r == nil {
		return
	}
	event := strings.TrimSpace(gjson.GetBytes(payload, "type").String())
	if isOpenAIWSTerminalEvent(event) || event == "error" {
		r.active.Store(false)
		select {
		case r.idleStart <- struct{}{}:
		default:
		}
	}
}

type openAIWSAdaptiveReaderKey struct{}

// The handler owns this lifetime across account failover attempts. Direct
// service callers get the same ownership for their single invocation.
func (s *OpenAIGatewayService) BeginOpenAIWSAdaptiveSession(ctx context.Context, conn *coderws.Conn) (context.Context, func()) {
	if _, ok := ctx.Value(openAIWSAdaptiveReaderKey{}).(*openAIWSAdaptiveReader); ok {
		return ctx, func() {}
	}
	ctx, cancel := context.WithCancelCause(ctx)
	reader := newOpenAIWSAdaptiveReader(ctx, cancel, conn, s.openAIWSIngressInterTurnIdleTimeout())
	return context.WithValue(ctx, openAIWSAdaptiveReaderKey{}, reader), func() { cancel(context.Canceled) }
}

// A real upstream generate=false response preserves IDs/usage, but must not
// commit the downstream session to passthrough before its first generating turn.
func (s *OpenAIGatewayService) proxyOpenAIWSAdaptiveWarmup(ctx context.Context, c *gin.Context, client *coderws.Conn, account *Account, token string, payload []byte, hooks *OpenAIWSIngressHooks, handoff *openAIWSAdaptiveHandoff) (*OpenAIForwardResult, error) {
	next := adaptivePassthroughHooks(hooks, handoff)
	next.adaptiveWarmup = true
	collector := &openAIWSToolCallReplayCollector{}
	next.adaptiveWarmupCollector = collector
	var result *OpenAIForwardResult
	next.AfterTurn = func(_ int, r *OpenAIForwardResult, _ error) { result = r }
	err := s.proxyResponsesWebSocketV2Passthrough(ctx, c, client, account, token, payload, next, s.getOpenAIWSProtocolResolver().Resolve(account))
	if result != nil {
		result.wsAccountFailoverReplayInput = collector.AllItems()
	}
	if err == nil && (result == nil || (result.UpstreamTerminalEvent != "response.completed" && result.UpstreamTerminalEvent != "response.done")) {
		err = NewOpenAIWSClientCloseError(coderws.StatusPolicyViolation, "adaptive warmup did not complete successfully", nil)
	}
	logOpenAIWSModeInfo("ingress_ws_adaptive_warmup_completed account_id=%d turn=%d payload_bytes=%d success=%v", account.ID, handoff.turn, len(payload), err == nil)
	return result, err
}
