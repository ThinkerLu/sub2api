package service

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"math"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"gopkg.in/yaml.v3"
)

const modelRegistryMaxBytes int64 = 4 << 20

// ModelRegistryCapability is an external capability profile selected by the
// model registry. It is intentionally independent from request routing: a
// registry model keeps its upstream name and only changes its advertised
// capability metadata.
type ModelRegistryCapability struct {
	DisplayName                 string
	Description                 string
	InputModalities             []string
	ContextWindow               int64
	MaxContextWindow            int64
	SupportsReasoning           *bool
	DefaultReasoningLevel       string
	SupportedReasoningLevels    []string
	SupportsParallelToolCalls   *bool
	SupportsImageDetailOriginal *bool
}

type ModelRegistryModel struct {
	ID         string
	Capability *ModelRegistryCapability
	Pricing    *ModelPricing
}

type modelRegistrySnapshot struct {
	models       map[string]ModelRegistryModel
	sourceSHA256 string
	loadedAt     time.Time
}

type modelRegistryDocument struct {
	Version            int                                       `yaml:"version"`
	Models             map[string]modelRegistryModelEntry        `yaml:"models"`
	CapabilityProfiles map[string]modelRegistryCapabilityProfile `yaml:"capability_profiles"`
	PricingProfiles    map[string]modelRegistryPricingProfile    `yaml:"pricing_profiles"`
}

type modelRegistryModelEntry struct {
	CapabilityProfile string `yaml:"capability_profile"`
	PricingProfile    string `yaml:"pricing_profile"`
}

type modelRegistryCapabilityProfile struct {
	DisplayName                 string   `yaml:"display_name"`
	Description                 string   `yaml:"description"`
	InputModalities             []string `yaml:"input_modalities"`
	ContextWindow               int64    `yaml:"context_window"`
	MaxContextWindow            int64    `yaml:"max_context_window"`
	SupportsReasoning           *bool    `yaml:"supports_reasoning"`
	DefaultReasoningLevel       string   `yaml:"default_reasoning_level"`
	SupportedReasoningLevels    []string `yaml:"supported_reasoning_levels"`
	SupportsParallelToolCalls   *bool    `yaml:"supports_parallel_tool_calls"`
	SupportsImageDetailOriginal *bool    `yaml:"supports_image_detail_original"`
}

type modelRegistryPricingProfile struct {
	InputPricePerToken                 float64            `yaml:"input_price_per_token"`
	InputPricePerTokenPriority         float64            `yaml:"input_price_per_token_priority"`
	ImageInputPricePerToken            float64            `yaml:"image_input_price_per_token"`
	ImageCacheReadPricePerToken        float64            `yaml:"image_cache_read_price_per_token"`
	OutputPricePerToken                float64            `yaml:"output_price_per_token"`
	OutputPricePerTokenPriority        float64            `yaml:"output_price_per_token_priority"`
	CacheCreationPricePerToken         float64            `yaml:"cache_creation_price_per_token"`
	CacheCreationPricePerTokenPriority float64            `yaml:"cache_creation_price_per_token_priority"`
	CacheReadPricePerToken             float64            `yaml:"cache_read_price_per_token"`
	CacheReadPricePerTokenPriority     float64            `yaml:"cache_read_price_per_token_priority"`
	CacheCreation5mPrice               float64            `yaml:"cache_creation_5m_price_per_token"`
	CacheCreation1hPrice               float64            `yaml:"cache_creation_1h_price_per_token"`
	FastMultiplier                     *float64           `yaml:"fast_multiplier"`
	FlexMultiplier                     *float64           `yaml:"flex_multiplier"`
	MaxReasoningEffortMultiplier       *float64           `yaml:"max_reasoning_effort_multiplier"`
	ReasoningEffortMultipliers         map[string]float64 `yaml:"reasoning_effort_multipliers"`
	SupportsCacheBreakdown             bool               `yaml:"supports_cache_breakdown"`
	LongContextInputThreshold          int                `yaml:"long_context_input_threshold"`
	LongContextThresholdInclusive      bool               `yaml:"long_context_threshold_inclusive"`
	LongContextInputMultiplier         float64            `yaml:"long_context_input_multiplier"`
	LongContextOutputMultiplier        float64            `yaml:"long_context_output_multiplier"`
	ImageOutputPricePerToken           float64            `yaml:"image_output_price_per_token"`
}

// ModelRegistry owns an immutable, atomically replaceable view of an external
// model metadata file. Reload failures never replace the last valid snapshot.
type ModelRegistry struct {
	enabled  bool
	path     string
	snapshot atomic.Pointer[modelRegistrySnapshot]
	reloadMu sync.Mutex
	// Lifecycle channels are retained for construction compatibility; reloading
	// is manual and no background watcher runs.
	stopCh chan struct{}
	doneCh chan struct{}
}

var processModelRegistry atomic.Pointer[ModelRegistry]

// CurrentModelRegistry returns the process-level registry. A nil result means
// the registry feature is disabled or has not been initialized.
func CurrentModelRegistry() *ModelRegistry {
	return processModelRegistry.Load()
}

// InitializeModelRegistry loads and publishes the process-level registry. When
// enabled, the initial file must be valid; callers should fail startup instead
// of silently reverting to unrelated local pricing and capability defaults.
func InitializeModelRegistry(cfg *config.Config) (*ModelRegistry, error) {
	registry := &ModelRegistry{}
	if cfg != nil && cfg.ModelRegistry.Enabled {
		registry.enabled = true
		registry.path = strings.TrimSpace(cfg.ModelRegistry.File)
		if registry.path == "" {
			return nil, errors.New("model_registry.file is required when model_registry.enabled is true")
		}
		if err := registry.Reload(); err != nil {
			return nil, fmt.Errorf("load model registry: %w", err)
		}
	}
	processModelRegistry.Store(registry)
	return registry, nil
}

func (r *ModelRegistry) Name() string { return "model registry" }

// SetCurrentModelRegistryForTest swaps the process-level registry and returns
// a restore function. It keeps tests isolated without exposing mutable state.
func SetCurrentModelRegistryForTest(registry *ModelRegistry) func() {
	previous := processModelRegistry.Swap(registry)
	return func() { processModelRegistry.Store(previous) }
}

func NewModelRegistryFromBytes(data []byte) (*ModelRegistry, error) {
	snapshot, err := parseModelRegistrySnapshot(data)
	if err != nil {
		return nil, err
	}
	registry := &ModelRegistry{}
	registry.snapshot.Store(snapshot)
	return registry, nil
}

func (r *ModelRegistry) Reload() error {
	if r == nil || !r.enabled {
		return nil
	}
	r.reloadMu.Lock()
	defer r.reloadMu.Unlock()

	info, err := os.Stat(r.path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", r.path, err)
	}
	if info.Size() > modelRegistryMaxBytes {
		return fmt.Errorf("model registry exceeds %d bytes", modelRegistryMaxBytes)
	}
	data, err := os.ReadFile(r.path)
	if err != nil {
		return fmt.Errorf("read %s: %w", r.path, err)
	}
	digest := sha256.Sum256(data)
	hash := hex.EncodeToString(digest[:])
	if current := r.snapshot.Load(); current != nil && current.sourceSHA256 == hash {
		return nil
	}
	snapshot, err := parseModelRegistrySnapshot(data)
	if err != nil {
		return err
	}
	snapshot.sourceSHA256 = hash
	snapshot.loadedAt = time.Now().UTC()
	r.snapshot.Store(snapshot)
	log.Printf("[ModelRegistry] loaded %d models from %s", len(snapshot.models), r.path)
	return nil
}

func (r *ModelRegistry) Model(modelID string) (ModelRegistryModel, bool) {
	if r == nil {
		return ModelRegistryModel{}, false
	}
	snapshot := r.snapshot.Load()
	if snapshot == nil {
		return ModelRegistryModel{}, false
	}
	model, ok := snapshot.models[normalizeModelRegistryKey(modelID)]
	return cloneModelRegistryModel(model), ok
}

func (r *ModelRegistry) Pricing(modelID string) (*ModelPricing, bool) {
	model, ok := r.Model(modelID)
	if !ok || model.Pricing == nil {
		return nil, false
	}
	return cloneModelPricing(model.Pricing), true
}

func (r *ModelRegistry) Capability(modelID string) (*ModelRegistryCapability, bool) {
	model, ok := r.Model(modelID)
	if !ok || model.Capability == nil {
		return nil, false
	}
	capability := *model.Capability
	capability.InputModalities = append([]string(nil), capability.InputModalities...)
	capability.SupportedReasoningLevels = append([]string(nil), capability.SupportedReasoningLevels...)
	if capability.SupportsReasoning != nil {
		value := *capability.SupportsReasoning
		capability.SupportsReasoning = &value
	}
	if capability.SupportsParallelToolCalls != nil {
		value := *capability.SupportsParallelToolCalls
		capability.SupportsParallelToolCalls = &value
	}
	if capability.SupportsImageDetailOriginal != nil {
		value := *capability.SupportsImageDetailOriginal
		capability.SupportsImageDetailOriginal = &value
	}
	return &capability, true
}

func (r *ModelRegistry) SnapshotInfo() map[string]any {
	if r == nil {
		return map[string]any{"enabled": false, "loaded": false}
	}
	snapshot := r.snapshot.Load()
	if snapshot == nil {
		return map[string]any{"enabled": r.enabled, "loaded": false}
	}
	return map[string]any{
		"enabled":       r.enabled,
		"loaded":        true,
		"models":        len(snapshot.models),
		"source_sha256": snapshot.sourceSHA256,
		"loaded_at":     snapshot.loadedAt,
	}
}

func parseModelRegistrySnapshot(data []byte) (*modelRegistrySnapshot, error) {
	var document modelRegistryDocument
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("parse model registry: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse trailing model registry content: %w", err)
	} else if err == nil {
		return nil, errors.New("model registry must contain one YAML document")
	}
	if document.Version != 0 && document.Version != 1 {
		return nil, fmt.Errorf("unsupported model registry version %d", document.Version)
	}

	capabilityProfiles := make(map[string]*ModelRegistryCapability, len(document.CapabilityProfiles))
	for rawName, profile := range document.CapabilityProfiles {
		name := normalizeModelRegistryKey(rawName)
		if name == "" {
			return nil, errors.New("capability profile name cannot be empty")
		}
		compiled, err := compileModelRegistryCapability(rawName, profile)
		if err != nil {
			return nil, err
		}
		capabilityProfiles[name] = compiled
	}

	pricingProfiles := make(map[string]*ModelPricing, len(document.PricingProfiles))
	for rawName, profile := range document.PricingProfiles {
		name := normalizeModelRegistryKey(rawName)
		if name == "" {
			return nil, errors.New("pricing profile name cannot be empty")
		}
		compiled, err := compileModelRegistryPricing(rawName, profile)
		if err != nil {
			return nil, err
		}
		pricingProfiles[name] = compiled
	}

	models := make(map[string]ModelRegistryModel, len(document.Models))
	for rawModelID, entry := range document.Models {
		modelID := strings.TrimSpace(rawModelID)
		key := normalizeModelRegistryKey(modelID)
		if key == "" {
			return nil, errors.New("model registry model name cannot be empty")
		}
		if _, exists := models[key]; exists {
			return nil, fmt.Errorf("duplicate model registry model %q", modelID)
		}
		capabilityName := normalizeModelRegistryKey(entry.CapabilityProfile)
		pricingName := normalizeModelRegistryKey(entry.PricingProfile)
		if capabilityName == "" && pricingName == "" {
			return nil, fmt.Errorf("model %q must reference capability_profile or pricing_profile", modelID)
		}
		model := ModelRegistryModel{ID: modelID}
		if capabilityName != "" {
			profile, ok := capabilityProfiles[capabilityName]
			if !ok {
				return nil, fmt.Errorf("model %q references unknown capability_profile %q", modelID, entry.CapabilityProfile)
			}
			model.Capability = profile
		}
		if pricingName != "" {
			profile, ok := pricingProfiles[pricingName]
			if !ok {
				return nil, fmt.Errorf("model %q references unknown pricing_profile %q", modelID, entry.PricingProfile)
			}
			model.Pricing = profile
		}
		models[key] = model
	}
	return &modelRegistrySnapshot{models: models}, nil
}

func compileModelRegistryCapability(name string, profile modelRegistryCapabilityProfile) (*ModelRegistryCapability, error) {
	modalities := normalizeCodexInputModalities(profile.InputModalities)
	if len(profile.InputModalities) > 0 && len(modalities) == 0 {
		return nil, fmt.Errorf("capability profile %q has no valid input_modalities", name)
	}
	if profile.ContextWindow < 0 || profile.MaxContextWindow < 0 {
		return nil, fmt.Errorf("capability profile %q context values must be non-negative", name)
	}
	if profile.ContextWindow > 0 && profile.MaxContextWindow > 0 && profile.MaxContextWindow < profile.ContextWindow {
		return nil, fmt.Errorf("capability profile %q max_context_window must be >= context_window", name)
	}
	levels := make([]string, 0, len(profile.SupportedReasoningLevels))
	seen := make(map[string]struct{}, len(profile.SupportedReasoningLevels))
	for _, raw := range profile.SupportedReasoningLevels {
		level := normalizeReasoningLevel(raw)
		if level == "" {
			return nil, fmt.Errorf("capability profile %q has invalid reasoning level %q", name, raw)
		}
		if _, exists := seen[level]; exists {
			continue
		}
		seen[level] = struct{}{}
		levels = append(levels, level)
	}
	defaultLevel := normalizeReasoningLevel(profile.DefaultReasoningLevel)
	if profile.DefaultReasoningLevel != "" && defaultLevel == "" {
		return nil, fmt.Errorf("capability profile %q has invalid default_reasoning_level", name)
	}
	if defaultLevel != "" && len(levels) > 0 {
		if _, ok := seen[defaultLevel]; !ok {
			return nil, fmt.Errorf("capability profile %q default_reasoning_level is not in supported_reasoning_levels", name)
		}
	}
	return &ModelRegistryCapability{
		DisplayName:                 strings.TrimSpace(profile.DisplayName),
		Description:                 strings.TrimSpace(profile.Description),
		InputModalities:             modalities,
		ContextWindow:               profile.ContextWindow,
		MaxContextWindow:            profile.MaxContextWindow,
		SupportsReasoning:           cloneBoolPointer(profile.SupportsReasoning),
		DefaultReasoningLevel:       defaultLevel,
		SupportedReasoningLevels:    levels,
		SupportsParallelToolCalls:   cloneBoolPointer(profile.SupportsParallelToolCalls),
		SupportsImageDetailOriginal: cloneBoolPointer(profile.SupportsImageDetailOriginal),
	}, nil
}

func compileModelRegistryPricing(name string, profile modelRegistryPricingProfile) (*ModelPricing, error) {
	values := []float64{
		profile.InputPricePerToken,
		profile.InputPricePerTokenPriority,
		profile.ImageInputPricePerToken,
		profile.ImageCacheReadPricePerToken,
		profile.OutputPricePerToken,
		profile.OutputPricePerTokenPriority,
		profile.CacheCreationPricePerToken,
		profile.CacheCreationPricePerTokenPriority,
		profile.CacheReadPricePerToken,
		profile.CacheReadPricePerTokenPriority,
		profile.CacheCreation5mPrice,
		profile.CacheCreation1hPrice,
		profile.LongContextInputMultiplier,
		profile.LongContextOutputMultiplier,
		profile.ImageOutputPricePerToken,
	}
	for _, value := range values {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return nil, fmt.Errorf("pricing profile %q contains an invalid non-negative price", name)
		}
	}
	for label, value := range map[string]*float64{
		"fast_multiplier":                 profile.FastMultiplier,
		"flex_multiplier":                 profile.FlexMultiplier,
		"max_reasoning_effort_multiplier": profile.MaxReasoningEffortMultiplier,
	} {
		if value != nil && (math.IsNaN(*value) || math.IsInf(*value, 0) || *value < 0) {
			return nil, fmt.Errorf("pricing profile %q has invalid %s", name, label)
		}
	}
	for effort, multiplier := range profile.ReasoningEffortMultipliers {
		if math.IsNaN(multiplier) || math.IsInf(multiplier, 0) || multiplier < 0 {
			return nil, fmt.Errorf("pricing profile %q has invalid reasoning_effort_multipliers.%s", name, effort)
		}
	}
	if profile.LongContextInputThreshold < 0 {
		return nil, fmt.Errorf("pricing profile %q long_context_input_threshold must be non-negative", name)
	}
	reasoningEffortMultipliers := maps.Clone(profile.ReasoningEffortMultipliers)
	if len(reasoningEffortMultipliers) == 0 && profile.MaxReasoningEffortMultiplier != nil {
		reasoningEffortMultipliers = map[string]float64{"max": *profile.MaxReasoningEffortMultiplier}
	}
	return &ModelPricing{
		InputPricePerToken:                 profile.InputPricePerToken,
		InputPricePerTokenPriority:         profile.InputPricePerTokenPriority,
		ImageInputPricePerToken:            profile.ImageInputPricePerToken,
		ImageCacheReadPricePerToken:        profile.ImageCacheReadPricePerToken,
		OutputPricePerToken:                profile.OutputPricePerToken,
		OutputPricePerTokenPriority:        profile.OutputPricePerTokenPriority,
		CacheCreationPricePerToken:         profile.CacheCreationPricePerToken,
		CacheCreationPricePerTokenPriority: profile.CacheCreationPricePerTokenPriority,
		CacheCreationPriceExplicit:         true,
		CacheReadPricePerToken:             profile.CacheReadPricePerToken,
		CacheReadPricePerTokenPriority:     profile.CacheReadPricePerTokenPriority,
		FastMultiplier:                     cloneFloatPointer(profile.FastMultiplier),
		FlexMultiplier:                     cloneFloatPointer(profile.FlexMultiplier),
		ReasoningEffortMultipliers:         reasoningEffortMultipliers,
		CacheCreation5mPrice:               profile.CacheCreation5mPrice,
		CacheCreation1hPrice:               profile.CacheCreation1hPrice,
		SupportsCacheBreakdown:             profile.SupportsCacheBreakdown,
		LongContextInputThreshold:          profile.LongContextInputThreshold,
		LongContextThresholdInclusive:      profile.LongContextThresholdInclusive,
		LongContextInputMultiplier:         profile.LongContextInputMultiplier,
		LongContextOutputMultiplier:        profile.LongContextOutputMultiplier,
		ImageOutputPricePerToken:           profile.ImageOutputPricePerToken,
		ImageOutputPriceExplicit:           true,
	}, nil
}

func applyModelRegistryCapability(descriptor *configuredCodexModelDescriptor, capability *ModelRegistryCapability) {
	if descriptor == nil || capability == nil {
		return
	}
	if capability.DisplayName != "" {
		descriptor.DisplayName = capability.DisplayName
	}
	if capability.Description != "" {
		descriptor.Description = capability.Description
	}
	if len(capability.InputModalities) > 0 {
		descriptor.InputModalities = append([]string(nil), capability.InputModalities...)
	}
	if capability.ContextWindow > 0 {
		descriptor.ContextWindow = capability.ContextWindow
	}
	if capability.MaxContextWindow > 0 {
		descriptor.MaxContextWindow = capability.MaxContextWindow
	}
	if capability.SupportsReasoning != nil {
		if !*capability.SupportsReasoning {
			none := "none"
			descriptor.DefaultReasoningLevel = &none
			descriptor.SupportedReasoningLevels = []configuredCodexReasoningLevel{{Effort: "none", Description: configuredCodexReasoningLevelDescription("none")}}
		} else if len(capability.SupportedReasoningLevels) > 0 {
			levels := make([]configuredCodexReasoningLevel, 0, len(capability.SupportedReasoningLevels))
			for _, effort := range capability.SupportedReasoningLevels {
				levels = append(levels, configuredCodexReasoningLevel{Effort: effort, Description: configuredCodexReasoningLevelDescription(effort)})
			}
			descriptor.SupportedReasoningLevels = levels
			if capability.DefaultReasoningLevel != "" {
				defaultLevel := capability.DefaultReasoningLevel
				descriptor.DefaultReasoningLevel = &defaultLevel
			}
		}
	} else if capability.DefaultReasoningLevel != "" && len(capability.SupportedReasoningLevels) > 0 {
		levels := make([]configuredCodexReasoningLevel, 0, len(capability.SupportedReasoningLevels))
		for _, effort := range capability.SupportedReasoningLevels {
			levels = append(levels, configuredCodexReasoningLevel{Effort: effort, Description: configuredCodexReasoningLevelDescription(effort)})
		}
		descriptor.SupportedReasoningLevels = levels
		defaultLevel := capability.DefaultReasoningLevel
		descriptor.DefaultReasoningLevel = &defaultLevel
	}
	if capability.SupportsParallelToolCalls != nil {
		descriptor.SupportsParallelToolCalls = *capability.SupportsParallelToolCalls
	}
	if capability.SupportsImageDetailOriginal != nil {
		descriptor.SupportsImageDetailOriginal = *capability.SupportsImageDetailOriginal
	}
}

func applyModelRegistryToCodexManifest(body []byte) ([]byte, error) {
	registry := CurrentModelRegistry()
	if registry == nil || len(body) == 0 {
		return body, nil
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		return body, nil
	}
	field := ""
	for _, candidate := range []string{"models", "data"} {
		if _, ok := envelope[candidate]; ok {
			field = candidate
			break
		}
	}
	if field == "" {
		return body, nil
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(envelope[field], &entries); err != nil {
		return body, nil
	}
	changed := false
	for i, raw := range entries {
		var entry map[string]json.RawMessage
		if err := json.Unmarshal(raw, &entry); err != nil || entry == nil {
			continue
		}
		modelID := ""
		for _, key := range []string{"slug", "id"} {
			if value := entry[key]; len(value) > 0 {
				_ = json.Unmarshal(value, &modelID)
				if strings.TrimSpace(modelID) != "" {
					break
				}
			}
		}
		capability, ok := registry.Capability(modelID)
		if !ok {
			continue
		}
		updated := applyModelRegistryCapabilityToRawMap(entry, capability)
		if !updated {
			continue
		}
		encoded, err := json.Marshal(entry)
		if err != nil {
			return nil, err
		}
		entries[i] = encoded
		changed = true
	}
	if !changed {
		return body, nil
	}
	encodedEntries, err := json.Marshal(entries)
	if err != nil {
		return nil, err
	}
	envelope[field] = encodedEntries
	return json.Marshal(envelope)
}

func applyModelRegistryCapabilityToRawMap(entry map[string]json.RawMessage, capability *ModelRegistryCapability) bool {
	if entry == nil || capability == nil {
		return false
	}
	changed := false
	setRaw := func(key string, value any) {
		encoded, err := json.Marshal(value)
		if err != nil || bytes.Equal(bytes.TrimSpace(entry[key]), bytes.TrimSpace(encoded)) {
			return
		}
		entry[key] = encoded
		changed = true
	}
	if capability.DisplayName != "" {
		setRaw("display_name", capability.DisplayName)
	}
	if capability.Description != "" {
		setRaw("description", capability.Description)
	}
	if len(capability.InputModalities) > 0 {
		setRaw("input_modalities", capability.InputModalities)
	}
	if capability.ContextWindow > 0 {
		setRaw("context_window", capability.ContextWindow)
	}
	if capability.MaxContextWindow > 0 {
		setRaw("max_context_window", capability.MaxContextWindow)
	}
	if capability.SupportsReasoning != nil {
		if !*capability.SupportsReasoning {
			setRaw("default_reasoning_level", nil)
			setRaw("supported_reasoning_levels", []configuredCodexReasoningLevel{{Effort: "none", Description: configuredCodexReasoningLevelDescription("none")}})
		} else if len(capability.SupportedReasoningLevels) > 0 {
			levels := make([]configuredCodexReasoningLevel, 0, len(capability.SupportedReasoningLevels))
			for _, effort := range capability.SupportedReasoningLevels {
				levels = append(levels, configuredCodexReasoningLevel{Effort: effort, Description: configuredCodexReasoningLevelDescription(effort)})
			}
			setRaw("supported_reasoning_levels", levels)
			if capability.DefaultReasoningLevel != "" {
				setRaw("default_reasoning_level", capability.DefaultReasoningLevel)
			}
		}
	} else if capability.DefaultReasoningLevel != "" && len(capability.SupportedReasoningLevels) > 0 {
		levels := make([]configuredCodexReasoningLevel, 0, len(capability.SupportedReasoningLevels))
		for _, effort := range capability.SupportedReasoningLevels {
			levels = append(levels, configuredCodexReasoningLevel{Effort: effort, Description: configuredCodexReasoningLevelDescription(effort)})
		}
		setRaw("supported_reasoning_levels", levels)
		setRaw("default_reasoning_level", capability.DefaultReasoningLevel)
	}
	if capability.SupportsParallelToolCalls != nil {
		setRaw("supports_parallel_tool_calls", *capability.SupportsParallelToolCalls)
	}
	if capability.SupportsImageDetailOriginal != nil {
		setRaw("supports_image_detail_original", *capability.SupportsImageDetailOriginal)
	}
	return changed
}

func normalizeModelRegistryKey(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func cloneBoolPointer(value *bool) *bool {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneFloatPointer(value *float64) *float64 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneModelRegistryModel(model ModelRegistryModel) ModelRegistryModel {
	cloned := model
	if model.Capability != nil {
		capability := *model.Capability
		cloned.Capability = &capability
	}
	if model.Pricing != nil {
		cloned.Pricing = cloneModelPricing(model.Pricing)
	}
	return cloned
}

func cloneModelPricing(pricing *ModelPricing) *ModelPricing {
	if pricing == nil {
		return nil
	}
	cloned := *pricing
	cloned.FastMultiplier = cloneFloatPointer(pricing.FastMultiplier)
	cloned.FlexMultiplier = cloneFloatPointer(pricing.FlexMultiplier)
	cloned.ReasoningEffortMultipliers = maps.Clone(pricing.ReasoningEffortMultipliers)
	return &cloned
}
