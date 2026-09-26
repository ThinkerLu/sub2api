# 按目标模型保留 Responses reasoning content

- 目的：OpenAI 平台的兼容 API Key 账号映射到 DeepSeek/Kimi 等要求思考回传的模型时，保留 `input[].reasoning.content[].reasoning_text`，避免按账号平台统一删除。
- 使用：账号配置 `model_mapping` 后，发往该账号的 Responses HTTP 或 WS 请求按映射后的目标模型分类；目标为 `ResolveThinkingProtocol` 的 `PassbackRequired` 时保留思考正文。真实 OpenAI 目标维持原有的清理规则。
- 行为：`store=false` 的 API Key 请求同样跳过针对 OpenAI 密文的 reasoning 过滤。既有归一化函数继续作用于其他目标；净化幂等，原始请求历史不因这一步被就地修改。
- 已知异厂商密文在提交前清理：passback 目标删除 OpenAI `gAAAAA` reasoning，已知 GPT 目标删除第三方 `-0` 占位 reasoning；未知格式不猜测。passback 目标遇到不可移植 compaction 密文时拒绝，不静默丢失压缩历史。跨账号的严格全量副本处理见切模型恢复功能文档。
- 限制：仅解决同账号目标模型的思考正文丢失；不保证跨账号切模型、加密 reasoning 可移植性、工具链完整性或客户端重连。不增加可见推理转文本。未知协议仍沿用现有 OpenAI 兼容规则。
- 配置/API/迁移：不增加配置、外部 API 或数据库迁移。`model_mapping` 的配置格式不变。
- 验证：运行 `go test ./internal/service -run 'TestNormalizeOpenAIResponsesReasoningForFinalModel|TestOpenAICompatibilityPreservesPassbackReasoningWithStoreFalse|TestAdaptiveChannelMappedPassbackReasoning|TestPassthroughLifecycle_LaterTurnChannelMappedPassbackReasoning' -count=1`，并运行完整 service 测试。fake upstream 已核对渠道别名映射后的 HTTP bridge 和 passthrough 第二轮发包；真实第三方多轮与缓存前缀仍需灰度验证。
- 发布/回滚：目标版本暂定 `v0.2.8-beta1-dual-leg-thinking-ws`；仅在整体双链路与迁移验收通过后从发布分支打包。回滚应用镜像即可恢复旧归一化行为，无数据迁移；切回旧行为可能再次丢失第三方思考正文。
- 关联提交：`25958bbbb`（目标模型归一化）、`codex/dual-leg-thinking-ws-cc` 后续 WS 渠道映射顺序修复提交；合入发布分支时保留对应提交记录。

关联实现提交：`25958bbbb`、`0a031713f`、`34377b6ba`、`89c098eb6`。
