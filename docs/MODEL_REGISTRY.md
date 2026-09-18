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
MODEL_REGISTRY_RELOAD_INTERVAL_SECONDS=5
```

Keep the YAML file in a host directory and mount the directory read-only:

```yaml
volumes:
  - ./model-registry:/app/model-registry:ro
```

The file is not embedded in the image. Directory mounts are used instead of a
single-file bind mount so replacing the file on the host remains visible inside
the container.

## Schema

```yaml
version: 1
models:
  deepseek-flash:
    capability_profile: deepseek-flash-vision
    pricing_profile: deepseek-flash-vision

capability_profiles:
  deepseek-flash-vision:
    display_name: DeepSeek V4.1 Flash
    description: DeepSeek coding and reasoning model.
    input_modalities: [text, image]
    context_window: 1048576
    max_context_window: 1048576
    supports_reasoning: true
    default_reasoning_level: high
    supported_reasoning_levels: [low, high, max]
    supports_parallel_tool_calls: true
    supports_image_detail_original: false

pricing_profiles:
  deepseek-flash-vision:
    input_price_per_token: 0.00000015
    output_price_per_token: 0.00000060
    cache_read_price_per_token: 0.000000003
```

All prices are USD per token. A model entry may select only one of the two
profiles when only capability or pricing needs an override.

## Reload

The service checks the file every configured interval. A valid change is
compiled into an immutable snapshot and swapped atomically. Invalid YAML,
unknown profile references, negative prices, or invalid modalities are
rejected; the previous valid snapshot remains active.

For an immediate reload without restarting the service:

```sh
docker compose -f /home/thinkerlu/sub2api-deploy/docker-compose.yml kill -s HUP sub2api
```

The release package also provides `reload.sh`.

## Precedence

Model listing and Codex manifests use the registry capability before returning
the final model descriptor. Pricing resolution uses the registry as the base
price, then applies group and channel pricing overrides. Group and channel
explicit pricing therefore remain higher priority than the registry profile.
