# WS 切模型的受控全量重连

> beta2 更新：新增环境开关、恢复灰度配置和内存预算已移除；以下为 beta1 历史设计。当前行为见 [双链路默认生效](2026-09-26-dual-leg-default-no-budget.md)。

目标版本：`v0.2.8-beta1-dual-leg-thinking-ws`（待发布）。关联分支提交 `feat(ws): recover model switches through one-shot full-context retries`。

## 使用与范围

在已启用双链路和有效内存额度的基础上，配置 `gateway.openai_ws.model_switch_full_retry_enabled=true`，并显式设置 `model_switch_allowed_group_ids` 与 `model_switch_allowed_client_versions`。默认关闭，空 allowlist 不启用。当前验证版本为 Codex CLI `0.155.0-alpha.16.4`，客户端首段 User-Agent 必须为对应 `codex_cli_rs/<version>`。

入口对具备兼容 Responses 档案的受管账号启用跨账号恢复，包括原生 HTTP Responses 目标；跨平台通过 composite 分组既有模型归属规则约束。旧账号仍符合目标平台/模型策略时保持账号；不符合且请求模型确实变化时，核对上一终态 response 的 user/API key/group/会话作用域，再冻结目标账号与平台。原始增量帧不提交上游，向客户端返回标准 `previous_response_not_found`，保留 `stream_id`。

## 重连与历史安全

重连需相同认证主体与可识别线程/会话、空旧 ID、相同目标模型，且 full input 的尾段摘要匹配被拒绝的增量。请求用共享 CAS 认领一次，目标账号不可被旧 sticky 或通用调度替换；目标策略变化、重复、TTL 到期、缓存异常或后续失败均不再自动重播。意图存活 90 秒，完整重发的去重记录保留两分钟；切回模型可以换新意图，上一请求的去重记录仍有效。

客户端切模型已经发送空旧 ID 的全量帧时，只在本连接保存了已确认的上一终态归属、内容可移植且仍 NOT_SENT 时，注册相同的一次性目标意图并正常关闭；不对不存在的旧 ID 发送缓存错误。已验证 Codex 会以全量上下文重连。实际客户端探针同时核对重连前后的执行作用域与输入摘要一致。

目标副本保留用户/助手消息及按顺序配对的 function/custom call/output，跨账号丢弃不可移植 reasoning，不将思考转成普通文本。拒绝孤立工具输出、未完成工具对、重复 call ID、旧文件/向量库/容器 ID、item_reference、compaction 密文及未适配的托管工具。输入尾段匹配并不证明任意客户端持有完整历史，因此仅对经过验证的客户端版本启用。

同账号在 GPT 与 passback 协议族之间切模型时，已有 bridge 历史可在本地验证并净化，关闭旧上游 WS 后以全量新链继续，不触发客户端切号重连。旧 response ID 的账号归属不改绑。

## 用量、配置和数据

本地要求全量重发的拒绝不产生上游 attempt 或账单。handler 测试验证 GPT→DeepSeek→GPT 三次实际请求对应三条 usage；两次切模型拒绝及一次重复重连均不加账单。已提交失败如有观测 usage，双链路保留计费依据，不伪造零成本。

新增三个配置项，不增加数据库迁移或公开 API。使用独立 Redis owner/intent/replay 键。历史缺少严格 owner 的响应不能自动迁移。当前完整代理历史直接跨账号执行的优化尚未启用，跨账号统一经过受控客户端全量重连。

## 验证

- `go test ./internal/service ./internal/repository ./internal/handler -run '^TestWSRecovery|^TestWSMigration|^TestWSModelMigrationHandler|^TestDualLeg|^TestTargetReasoning' -count=1`
- 相同迁移测试范围运行 `go test -race`；完整 service/config 与 WS handler 回归。
- `SUB2API_CODEX_PROBE_BIN=<codex路径> go test ./internal/service -run '^TestCodexWSRecoveryProbe$' -count=1 -v`：隔离 CLI 家目录、dummy key、本地假上游，不执行模型工具。实测两条 WS，增量 input 3 项，重连 full input 3 项，旧 ID 为空且合成提示保留。此短上下文探针不代替长历史、压缩和所有工具形式的验证。

## 发布和回滚

先以配置关闭状态交付备用机发布包，灰度仅覆盖 allowlist 的组、客户端版本和已适配账号。停止新迁移时关闭创建开关，保留双链路的迁移状态读取，直到已产生的意图与去重窗口失效；不得清空 Redis 状态以强行恢复。回滚到不识别恢复状态的旧镜像前需要停止新 WS 并等待两分钟窗口排空，保留业务数据及旧 owner 记录。

关联实现提交：`34377b6ba`、`89c098eb6`。
