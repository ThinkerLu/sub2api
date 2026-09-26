package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenAIWSCompositeRouteMatchesHTTPResolver(t *testing.T) {
	for _, tc := range []struct {
		name      string
		model     string
		ownership CompositeModelOwnership
		lookupErr error
		routes    []CompositeModelRoute
	}{
		{name: "openai owns deepseek name", model: "deepseek-flash", ownership: CompositeModelOwnership{Matched: true, TargetPlatform: PlatformOpenAI}},
		{name: "real deepseek account", model: "deepseek-flash", ownership: CompositeModelOwnership{Matched: true, TargetPlatform: PlatformDeepseek}},
		{name: "ambiguous recognizable name", model: "deepseek-flash", ownership: CompositeModelOwnership{Ambiguous: true}},
		{name: "unknown alias", model: "private-alias"},
		{name: "catalog failure", model: "private-alias", lookupErr: errors.New("catalog unavailable")},
		{name: "explicit response route wins", model: "deepseek-flash", ownership: CompositeModelOwnership{Matched: true, TargetPlatform: PlatformDeepseek}, routes: []CompositeModelRoute{
			{ID: 1, GroupID: 2, PublicModel: "deepseek-flash", MatchType: CompositeRouteMatchExact, TargetPlatform: PlatformOpenAI, UpstreamModel: "private-upstream", Endpoint: CompositeRouteEndpointResponses, Enabled: true},
			{ID: 2, GroupID: 2, PublicModel: "deepseek-flash", MatchType: CompositeRouteMatchExact, TargetPlatform: PlatformDeepseek, UpstreamModel: "wrong-endpoint", Endpoint: CompositeRouteEndpointChatCompletions, Enabled: true},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolver := NewCompositeRouteResolver(compositeRouteRepoStub{routes: tc.routes})
			resolver.SetModelOwnershipResolver(func(_ context.Context, groupID int64, model string) (CompositeModelOwnership, error) {
				require.Equal(t, int64(2), groupID)
				require.Equal(t, tc.model, model)
				return tc.ownership, tc.lookupErr
			})
			s := &OpenAIGatewayService{}
			s.SetCompositeRouteResolver(resolver)
			// A previous turn's platform must never mask a new lookup or ambiguity.
			ctx := WithCompositeRouteDecision(context.Background(), CompositeRouteDecision{Matched: true, TargetPlatform: PlatformGrok, PublicModel: "grok-old", UpstreamModel: "grok-old"})
			want, httpErr := resolver.Resolve(ctx, 2, tc.model, CompositeRouteEndpointResponses)
			got, model, err := s.ResolveOpenAIWSRoute(ctx, &Group{ID: 2, Platform: PlatformComposite}, tc.model)
			if httpErr != nil || !want.Matched {
				require.Error(t, err)
				require.Empty(t, model)
				return
			}
			require.NoError(t, err)
			require.Equal(t, want.UpstreamModel, model)
			platform, ok := ResolvedTargetPlatformFromContext(got)
			require.True(t, ok)
			require.Equal(t, want.TargetPlatform, platform)
			public, _ := RequestedPublicModelFromContext(got)
			require.Equal(t, tc.model, public)
			source, _ := CompositeRouteSourceFromContext(got)
			require.Equal(t, want.Source, source)
		})
	}
}

func TestOpenAIWSRouteNonCompositeDoesNotResolve(t *testing.T) {
	resolver := NewCompositeRouteResolver(nil)
	resolver.SetModelOwnershipResolver(func(context.Context, int64, string) (CompositeModelOwnership, error) {
		t.Fatal("ordinary OpenAI group must not use composite routing")
		return CompositeModelOwnership{}, nil
	})
	s := &OpenAIGatewayService{compositeResolver: resolver}
	ctx := context.Background()
	got, model, err := s.ResolveOpenAIWSRoute(ctx, &Group{ID: 2, Platform: PlatformOpenAI}, "deepseek-flash")
	require.NoError(t, err)
	require.Equal(t, ctx, got)
	require.Equal(t, "deepseek-flash", model)
}
