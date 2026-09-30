# v0.2.5 外置模型注册表发布

本发布在现有 v0.2.5 分支上增加方案 E：模型注册表不改变上游请求模型名，而是为模型配置独立的能力档案和价格档案。

注册表文件位于部署目录的 `model-registry/model-registry.yaml`，不会写入镜像或程序目录。Compose 以只读目录方式挂载：

```yaml
volumes:
  - ./model-registry:/app/model-registry:ro
```

服务启动时读取注册表；默认每 5 秒检测文件变化，校验通过后原子替换当前快照。也可以不重启服务立即重载：

```sh
sudo ./reload.sh
```

重载失败时保留上一份有效快照，不会清空能力或价格配置。

## 配置示例

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
    input_modalities: [text, image]
    context_window: 1050000
    max_context_window: 1050000
    supports_reasoning: true
    default_reasoning_level: medium
    supported_reasoning_levels: [low, medium, high, xhigh, max]
    supports_parallel_tool_calls: true
  gpt-6-luna:
    input_modalities: [text, image]
    context_window: 1050000
    max_context_window: 1050000
    supports_reasoning: true
    default_reasoning_level: medium
    supported_reasoning_levels: [low, medium, high, xhigh, max]
    supports_parallel_tool_calls: true
  gpt-6.1-sol:
    input_modalities: [text, image]
    context_window: 1050000
    max_context_window: 1050000
    supports_reasoning: true
    default_reasoning_level: medium
    supported_reasoning_levels: [low, medium, high, xhigh, max, ultra]
    supports_parallel_tool_calls: true
  deepseek-v4-flash-vision-exp:
    input_modalities: [text, image]
    context_window: 1048576
    max_context_window: 1048576
    supports_reasoning: true
    default_reasoning_level: high
    supported_reasoning_levels: [low, medium, high, xhigh, max, ultra]
  glm-5.3-flash:
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

语义为：

```text
Codex 可见模型名：deepseek-flash
上游请求模型名：deepseek-flash
能力来源：deepseek-v4-flash-vision-exp capability profile
计费模型身份：deepseek-flash
价格来源：deepseek-v4-flash-vision-exp pricing profile
```

部署脚本只在文件不存在时复制示例，已有运营配置不会被覆盖。组价和渠道显式定价仍高于注册表基础价。

## 部署与回滚

进入本发布目录后执行：

```sh
sha256sum -c SHA256SUMS
sudo python3 release.py preflight
sudo ./deploy.sh

# 外部文件修改后立即重载
sudo ./reload.sh

# 回滚
sudo ./rollback.sh
```

默认 Compose 目录为 `/home/thinkerlu/sub2api-deploy`，可用 `--deploy-dir` 指定。部署前保存当前实际容器、Compose 和 `.env`；回滚恢复部署前版本，但保留数据库、业务数据和外置注册表目录。

## 验证

```sh
python3 -m unittest discover -s . -p test_release.py -v
```

发布脚本测试覆盖外部注册表保留、Compose 挂载、SIGHUP 重载和回滚时移除挂载。后端测试覆盖注册表解析、能力覆盖、价格优先解析和无效重载保留旧快照。
