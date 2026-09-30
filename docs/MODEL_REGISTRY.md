# External Model Registry

The model registry is an optional runtime overlay for model capabilities and
pricing. It does not rewrite the model name sent upstream. For a model `b`, the
registry can select a capability profile and a pricing profile without changing
the public model name or the upstream request body.

## Configuration

Enable the registry with environment variables:

```sh
MODEL_REGISTRY_ENABLED=true
MODEL_REGISTRY_FILE=/app/model-registry/model-registry.yaml
```

Keep the YAML file in a host directory and mount the directory read-only:

```yaml
volumes:
  - ./model-registry:/app/model-registry:ro
```

The file is not embedded in the image. Directory mounts are used instead of a
single-file bind mount so replacing the file on the host remains visible inside
the container.

The release worktree keeps an operational snapshot at
`deploy/model-registry/model-registry.yaml`. Update that snapshot together with
the target host's `model-registry/model-registry.yaml` so local review and
runtime configuration do not drift.

## Schema

```yaml
version: 1
models:
  gpt-6-sol:
    capability_profile: gpt-6-sol
  gpt-6-luna:
    capability_profile: gpt-6-luna
  gpt-6.1-sol:
    capability_profile: gpt-6.1-sol
  deepseek-flash:
    capability_profile: deepseek-v4-flash-vision-exp
    pricing_profile: deepseek-v4-flash-vision-exp
  glm-5.3-flash:
    capability_profile: glm-5.3-flash
    pricing_profile: glm-5.3-flash

capability_profiles:
  gpt-6-sol:
    display_name: GPT-6 Sol
    description: OpenAI GPT-6 Sol coding model.
    input_modalities: [text, image]
    context_window: 1050000
    max_context_window: 1050000
    supports_reasoning: true
    default_reasoning_level: medium
    supported_reasoning_levels: [low, medium, high, xhigh, max]
    supports_parallel_tool_calls: true
  gpt-6-luna:
    display_name: GPT-6 Luna
    description: OpenAI GPT-6 Luna coding model.
    input_modalities: [text, image]
    context_window: 1050000
    max_context_window: 1050000
    supports_reasoning: true
    default_reasoning_level: medium
    supported_reasoning_levels: [low, medium, high, xhigh, max]
    supports_parallel_tool_calls: true
  gpt-6.1-sol:
    display_name: GPT-6.1 Sol
    description: OpenAI GPT-6.1 Sol coding model.
    input_modalities: [text, image]
    context_window: 1050000
    max_context_window: 1050000
    supports_reasoning: true
    default_reasoning_level: medium
    supported_reasoning_levels: [low, medium, high, xhigh, max, ultra]
    supports_parallel_tool_calls: true
  deepseek-v4-flash-vision-exp:
    display_name: DeepSeek V4.1 Flash
    description: DeepSeek coding and reasoning model.
    input_modalities: [text, image]
    context_window: 1048576
    max_context_window: 1048576
    supports_reasoning: true
    default_reasoning_level: high
    supported_reasoning_levels: [low, medium, high, xhigh, max, ultra]
    supports_parallel_tool_calls: true
    supports_image_detail_original: false
  glm-5.3-flash:
    display_name: GLM-5.3-Flash
    description: Native multimodal GLM coding model.
    input_modalities: [text, image]
    context_window: 1000000
    max_context_window: 1000000
    supports_reasoning: true
    default_reasoning_level: high
    supported_reasoning_levels: [low, high, max]

pricing_profiles:
  deepseek-v4-flash-vision-exp:
    input_price_per_token: 0.00000015
    output_price_per_token: 0.00000060
    cache_read_price_per_token: 0.000000003
  glm-5.3-flash:
    input_price_per_token: 0.00000015
    output_price_per_token: 0.00000050
    cache_read_price_per_token: 0.00000003
```

All prices are USD per token. A model entry may select only one of the two
profiles when only capability or pricing needs an override.

## Reload

The service loads the file at startup and does not poll it on a timer. After
editing the file, trigger the shared manual reload signal. The same operation
also reloads the adaptive WS context policy, so both files are refreshed by one
command.

From a deployment directory containing `reload.sh`:

```sh
sudo ./reload.sh
```

The script sends `SIGHUP` to the container. A valid change is compiled into an
immutable snapshot and swapped atomically. Invalid YAML, unknown profile
references, negative prices, or invalid modalities are rejected; the previous
valid snapshot remains active. A failure in one runtime configuration does not
prevent the other configuration from being reloaded.

## Precedence

Model listing and Codex manifests use the registry capability before returning
the final model descriptor. Pricing resolution uses the registry as the base
price, then applies group and channel pricing overrides. Group and channel
explicit pricing therefore remain higher priority than the registry profile.
