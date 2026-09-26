# GPT 目标模型的 Responses WS 阈值选路

- 目的：区分 GPT 文本/推理模型与其他目标模型，让 15 MiB 只作为前者的 WS/HTTP 选路阈值，而不是任意模型的内存上限。
- 使用：`gateway.openai_ws.gpt_threshold_routing_enabled` 默认 `false`，关闭时保持旧的全模型阈值行为。未来完成共享内存准入、上下文恢复与逐 turn 兼容性验收后，才可开启；`http_bridge_threshold_bytes` 保持已有可配置字节阈值。
- 行为：开启后仅对列入代码内 `openAIWSGPTThresholdModels` 的最终上游模型应用阈值；客户端别名先经渠道及账号映射。ctx_pool 首帧比较原始与改写体，adaptive handoff 额外比较恢复候选体。任一候选体达到阈值时，按原有兼容条件候选 HTTP。Grok 强制 HTTP 等独立规则不变。
- 限制：passthrough 首帧仍只在进入 relay 前按原始帧判定，尚未预估之后的全部改写体。未知模型（包括 `gpt-oss`、`gpt-image`）不因 15 MiB 强制 HTTP，但不代表支持无限帧或无限内存。当前实现不单独保证 WS→HTTP→WS 逐 turn 往返、跨账号切换或资源安全；该配置保持关闭直到这些能力验收。模型名单扩充须经上游协议复核。
- 配置/API/迁移：新增网关布尔配置，不更改外部 API 与数据库；旧配置无须迁移。仅 `http_bridge_enabled` 已开启时首帧可因阈值进入 HTTP bridge。
- 验证：运行 `go test ./internal/service -run 'TestGPTThresholdRoutingUsesFinalModelAndLargestPayload|TestOpenAIWSPassthroughFirstMessageBridgeDecision|TestAdaptiveThresholdBoundary' -count=1`、`go test ./internal/config -run '^TestLoadDefaultOpenAIWSConfig$' -count=1`，以及完整 service 测试。上线前还需对压缩帧、replay 大于首帧和目标账号协议逐项做 fake upstream 测试及内存压测。
- 发布/回滚：目标版本暂定 `v0.2.10-dual-leg-thinking-ws`；该阶段不单独发布。回滚可关闭配置，完整发布需从干净发布分支构建并提供备用机基线匹配的回滚镜像。
- 关联提交：`codex/dual-leg-thinking-ws-cc` 的 `feat(ws): gate GPT-sized bridge routing by final model`。
