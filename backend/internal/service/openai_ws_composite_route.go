package service

import (
	"context"
	"fmt"
	"strings"
)

// SetCompositeRouteResolver shares the HTTP resolver and its account ownership catalog.
func (s *OpenAIGatewayService) SetCompositeRouteResolver(resolver *CompositeRouteResolver) {
	s.compositeResolver = resolver
}

// ResolveOpenAIWSRoute resolves each client model afresh, including model switches.
// The frame retains its public model for admission, billing and recovery identity.
func (s *OpenAIGatewayService) ResolveOpenAIWSRoute(ctx context.Context, group *Group, model string) (context.Context, string, error) {
	if group == nil || group.Platform != PlatformComposite {
		return ctx, model, nil
	}
	decision, err := s.compositeResolver.Resolve(ctx, group.ID, model, CompositeRouteEndpointResponses)
	if err != nil {
		return ctx, "", err
	}
	if !decision.Matched {
		return ctx, "", fmt.Errorf("%w: unresolved composite model route", ErrOpenAIWSRecoveryRejected)
	}
	upstream := strings.TrimSpace(decision.UpstreamModel)
	if upstream == "" {
		upstream = model
	}
	return WithCompositeRouteDecision(ctx, decision), upstream, nil
}
