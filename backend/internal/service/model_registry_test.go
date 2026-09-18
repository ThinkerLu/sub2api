package service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

const modelRegistryFixture = `
version: 1
models:
  deepseek-flash:
    capability_profile: deepseek-flash-vision
    pricing_profile: deepseek-flash-vision
capability_profiles:
  deepseek-flash-vision:
    display_name: DeepSeek V4.1 Flash
    description: Registry capability profile.
    input_modalities: [text, image]
    context_window: 1048576
    max_context_window: 1048576
    supports_reasoning: true
    default_reasoning_level: high
    supported_reasoning_levels: [low, high, max]
    supports_parallel_tool_calls: true
pricing_profiles:
  deepseek-flash-vision:
    input_price_per_token: 0.00000123
    output_price_per_token: 0.00000456
    cache_read_price_per_token: 0.000000078
`

func TestModelRegistryAppliesCapabilityAndPricing(t *testing.T) {
	registry, err := NewModelRegistryFromBytes([]byte(modelRegistryFixture))
	require.NoError(t, err)
	restore := SetCurrentModelRegistryForTest(registry)
	t.Cleanup(restore)

	capability, ok := registry.Capability("DEEPSEEK-FLASH")
	require.True(t, ok)
	require.Equal(t, []string{"text", "image"}, capability.InputModalities)
	require.Equal(t, int64(1048576), capability.ContextWindow)

	pricing, err := NewBillingService(&config.Config{}, nil).GetModelPricing("deepseek-flash")
	require.NoError(t, err)
	require.Equal(t, 1.23e-6, pricing.InputPricePerToken)
	require.Equal(t, 4.56e-6, pricing.OutputPricePerToken)
	require.Equal(t, 7.8e-8, pricing.CacheReadPricePerToken)

	body, err := applyModelRegistryToCodexManifest([]byte(`{"models":[{"slug":"deepseek-flash","input_modalities":["text"],"context_window":1000}]}`))
	require.NoError(t, err)
	var envelope struct {
		Models []map[string]any `json:"models"`
	}
	require.NoError(t, json.Unmarshal(body, &envelope))
	require.Len(t, envelope.Models, 1)
	require.Equal(t, []any{"text", "image"}, envelope.Models[0]["input_modalities"])
	require.Equal(t, float64(1048576), envelope.Models[0]["context_window"])
	require.Equal(t, "high", envelope.Models[0]["default_reasoning_level"])
}

func TestModelRegistryReloadKeepsLastValidSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model-registry.yaml")
	require.NoError(t, os.WriteFile(path, []byte(modelRegistryFixture), 0o600))
	registry := &ModelRegistry{
		enabled: true,
		path:    path,
		stopCh:  make(chan struct{}),
		doneCh:  make(chan struct{}),
	}
	require.NoError(t, registry.Reload())

	_, ok := registry.Pricing("deepseek-flash")
	require.True(t, ok)
	require.Error(t, registry.reloadBytesForTest([]byte("version: 1\nmodels:\n  deepseek-flash:\n    pricing_profile: missing\n")))

	_, ok = registry.Pricing("deepseek-flash")
	require.True(t, ok, "invalid reload must keep the previous valid snapshot")
}

func TestModelRegistryCapabilityOverridesGeneratedCodexManifest(t *testing.T) {
	registry, err := NewModelRegistryFromBytes([]byte(modelRegistryFixture))
	require.NoError(t, err)
	restore := SetCurrentModelRegistryForTest(registry)
	t.Cleanup(restore)

	const groupID int64 = 881
	service := &GatewayService{accountRepo: codexModelsVisibilityAccountRepo{byGroup: map[int64][]Account{
		groupID: {{
			ID:       1,
			Platform: PlatformOpenAI,
			Type:     AccountTypeAPIKey,
			Credentials: map[string]any{
				"model_mapping": map[string]any{"deepseek-flash": "deepseek-flash"},
			},
		}},
	}}}
	body, err := service.BuildCodexModelsManifestForGroup(
		context.Background(),
		&Group{ID: groupID, Platform: PlatformOpenAI},
		"",
		[]string{"deepseek-flash"},
	)
	require.NoError(t, err)
	models := decodeCodexManifestModels(t, body)
	require.Len(t, models, 1)
	require.Equal(t, []any{"text", "image"}, models[0]["input_modalities"])
	require.Equal(t, float64(1048576), models[0]["context_window"])
}

func (r *ModelRegistry) reloadBytesForTest(data []byte) error {
	snapshot, err := parseModelRegistrySnapshot(data)
	if err != nil {
		return err
	}
	r.snapshot.Store(snapshot)
	return nil
}
