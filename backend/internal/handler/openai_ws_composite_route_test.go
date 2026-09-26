package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type wsCompositeRouteRepo struct {
	service.CompositeModelRouteRepository
	routes []service.CompositeModelRoute
}

func TestResponsesWebSocketCompositeAmbiguityFailsClosed(t *testing.T) {
	var upstreamRequests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamRequests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()
	h := newOpenAIWSPassthroughHandlerHarness(t, upstream.URL)
	_ = h.clientConn.CloseNow()
	select {
	case <-h.handlerDone:
	case <-time.After(3 * time.Second):
		t.Fatal("unused connection did not close")
	}
	h.apiKey.Group = &service.Group{ID: *h.apiKey.GroupID, Platform: service.PlatformComposite}
	resolver := service.NewCompositeRouteResolver(nil)
	resolver.SetModelOwnershipResolver(func(context.Context, int64, string) (service.CompositeModelOwnership, error) {
		return service.CompositeModelOwnership{Ambiguous: true}, nil
	})
	h.gatewayService.SetCompositeRouteResolver(resolver)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(h.serverURL, "http")+"/openai/v1/responses", nil)
	require.NoError(t, err)
	defer conn.CloseNow()
	require.NoError(t, conn.Write(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"deepseek-flash","input":[]}`)))
	_, _, err = conn.Read(ctx)
	require.Equal(t, coderws.StatusPolicyViolation, coderws.CloseStatus(err))
	require.Zero(t, upstreamRequests.Load())
}

func (r wsCompositeRouteRepo) ListByGroup(context.Context, int64, bool) ([]service.CompositeModelRoute, error) {
	return r.routes, nil
}

func TestResponsesWebSocketCompositeRouting(t *testing.T) {
	for _, mode := range []string{service.OpenAIWSIngressModeAdaptive, service.OpenAIWSIngressModePassthrough, service.OpenAIWSIngressModeDedicated} {
		for _, explicit := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/explicit=%t", mode, explicit), func(t *testing.T) {
				upstreamFrames := make(chan string, 4)
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					conn, err := coderws.Accept(w, r, nil)
					if err != nil {
						return
					}
					defer conn.CloseNow()
					for {
						_, body, err := conn.Read(r.Context())
						if err != nil {
							return
						}
						model := gjson.GetBytes(body, "model").String()
						upstreamFrames <- model
						event := fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_composite","model":%q,"output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`, model)
						if conn.Write(r.Context(), coderws.MessageText, []byte(event)) != nil {
							return
						}
					}
				}))
				defer upstream.Close()
				h := newOpenAIWSPassthroughHandlerHarness(t, upstream.URL, func(cfg *config.Config, a *service.Account) {
					a.Extra["openai_apikey_responses_websockets_v2_mode"] = mode
					a.Credentials["model_mapping"] = map[string]any{"deepseek-flash": "deepseek-flash"}
				})
				_ = h.clientConn.CloseNow()
				select {
				case <-h.handlerDone:
				case <-time.After(3 * time.Second):
					t.Fatal("unused connection did not close")
				}
				h.apiKey.Group = &service.Group{ID: *h.apiKey.GroupID, Platform: service.PlatformComposite, RateMultiplier: 1}
				publicModel := "deepseek-flash"
				resolver := service.NewCompositeRouteResolver(nil)
				if explicit {
					publicModel = "custom-alias"
					resolver = service.NewCompositeRouteResolver(wsCompositeRouteRepo{routes: []service.CompositeModelRoute{{
						ID: 1, GroupID: *h.apiKey.GroupID, PublicModel: publicModel, MatchType: service.CompositeRouteMatchExact,
						TargetPlatform: service.PlatformOpenAI, UpstreamModel: "deepseek-flash", Endpoint: service.CompositeRouteEndpointResponses, Enabled: true,
					}}})
				}
				resolver.SetModelOwnershipResolver(func(_ context.Context, _ int64, model string) (service.CompositeModelOwnership, error) {
					if model == "custom-alias" {
						return service.CompositeModelOwnership{Matched: true, TargetPlatform: service.PlatformDeepseek}, nil
					}
					return service.CompositeModelOwnership{Matched: true, TargetPlatform: service.PlatformOpenAI}, nil
				})
				h.gatewayService.SetCompositeRouteResolver(resolver)
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				// A stale hint must not reject a valid composite first frame or poison its session.
				conn, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(h.serverURL, "http")+"/openai/v1/responses", &coderws.DialOptions{HTTPHeader: http.Header{
					"X-Codex-Turn-Metadata": {`{"model":"gpt-unavailable"}`},
					"User-Agent":            {"codex_cli_rs/0.155.0"},
				}})
				require.NoError(t, err)
				defer conn.CloseNow()
				for turn := 0; turn < 2; turn++ {
					if turn == 1 {
						publicModel = "deepseek-flash"
					}
					payload := fmt.Sprintf(`{"type":"response.create","model":%q,"input":[],"stream":true}`, publicModel)
					require.NoError(t, conn.Write(ctx, coderws.MessageText, []byte(payload)))
					for {
						_, event, err := conn.Read(ctx)
						require.NoError(t, err, "turn %d", turn+1)
						if gjson.GetBytes(event, "type").String() == "response.completed" {
							break
						}
					}
					select {
					case model := <-upstreamFrames:
						require.Equal(t, "deepseek-flash", model)
					case <-ctx.Done():
						t.Fatal("upstream did not receive frame")
					}
					select {
					case usage := <-h.usageRepo.created:
						require.Equal(t, publicModel, usage.Model)
					case <-ctx.Done():
						t.Fatal("turn usage was not recorded")
					}
				}
			})
		}
	}
}

func TestWSClientRequestedModelUsesCurrentTurn(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	c.Request = c.Request.WithContext(service.WithCompositeRouteDecision(c.Request.Context(), service.CompositeRouteDecision{
		Matched: true, PublicModel: "first-alias", TargetPlatform: service.PlatformOpenAI, UpstreamModel: "deepseek-flash",
	}))
	require.Equal(t, "first-alias", clientRequestedModel(c, "second-alias"))
	c.Request.Header.Set("Connection", "Upgrade")
	c.Request.Header.Set("Upgrade", "websocket")
	require.Equal(t, "second-alias", clientRequestedModel(c, "second-alias"))
}
