# WS 严格响应归属与一次性迁移状态

> beta2 更新：新增环境开关、恢复灰度配置和内存预算已移除；以下为 beta1 历史设计。当前行为见 [双链路默认生效](2026-09-26-dual-leg-default-no-budget.md)。

目标版本：`v0.2.8-beta1-dual-leg-thinking-ws`（待发布）。关联开发分支 `codex/dual-leg-thinking-ws-cc` 的 `feat(ws): persist strict response owners and one-shot migration intents` 提交。

## 目的与使用

为跨账号模型切换保存可验证的响应归属和一次性重连意图。服务层通过 `OpenAIWSRecoveryCache` 使用共享 Redis，不使用本地状态降级。实际入口由独立的切模型恢复开关控制，此状态接口不单独启用迁移。

响应归属包含认证 user/API key/group、响应 ID、原账号、请求/上游模型、会话作用域及终态标记。读取必须精确匹配三项身份；旧 HTTP owner 的同用户多 key 互通规则不用于这里。记录不可跨账号覆盖，历史没有新格式 owner 时拒绝恢复。

迁移意图最多存活两分钟，要求原 turn 终态、下一 turn `NOT_SENT`、目标账号不同及请求模型变化。只保存身份、模型、目标、输入尾段摘要和逻辑 turn ID，不保存原始上下文、思考正文或密钥。消费用 Redis CAS，保留已消费标记及剩余 TTL，避免双连接重复认领。过期、身份不符、重复或缓存不可用均失败封闭。

## 配置、数据与验证

无外部 API 或数据库迁移。Redis 使用独立版本化键前缀 `openai:ws-recovery:v1:`，记录按 TTL 清理，旧粘性映射保留。

验证命令：`go test ./internal/service ./internal/repository -run '^TestWSRecovery|^TestWSMigration' -count=1`。用例覆盖精确身份隔离、不可覆盖的旧归属、意图篡改、单次消费、两实例并发 CAS、TTL 与已消费墓碑。后续入口集成还须验证真实 Codex 全量重发及目标账号冻结。

## 发布与回滚

随完整方案合入发布分支；单有状态接口不启用切号。回滚不删除旧 owner 或意图，不把已消费记录变回 pending。入口恢复功能将独立灰度，关闭新意图时仍须拒绝重复和无法关联的重连。

关联实现提交：`2200b1fdb`、`34377b6ba`、`89c098eb6`。
