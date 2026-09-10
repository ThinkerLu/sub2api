package service

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestOpenAIRequestViewScalarOwnership(t *testing.T) {
	body := []byte(`{"model":"gpt-5","stream":true,"prompt_cache_key":"session","previous_response_id":"resp_1","service_tier":"priority","reasoning":{"effort":"high"}}`)
	view := newOpenAIRequestView(body)
	for i := range body {
		body[i] = 'X'
	}
	require.Equal(t, "gpt-5", view.Model)
	require.Equal(t, "session", view.PromptCacheKey)
	require.Equal(t, "resp_1", view.PreviousResponseID)
	require.Equal(t, "priority", view.ServiceTier)
	require.Equal(t, "high", view.ReasoningEffort)
}
