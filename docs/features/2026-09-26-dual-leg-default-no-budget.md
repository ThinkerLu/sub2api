# 双链路默认生效与移除内存预算

> 2026-10-01 开发修订：明确的上游失败事件不再因 SENT_UNKNOWN 状态被改写。
> 无上游终止事件时的执行未知保护保留，见 [错误透传](2026-10-01-v0211-upstream-errors.md)。

目标版本：`v0.2.8-beta2-dual-leg-default`。分支：`codex/dual-leg-default-no-budget`，关联实现提交由本分支和发布包 manifest 记录。官方基线保持 `v0.2.8`。

关联功能提交：`2ce748dd3`。发布合并提交和最终镜像摘要见 manifest。

## 变更与配置

移除 beta1 新增的全部七项环境配置及对应 config.yaml 字段：

| 删除的环境配置 | beta2 行为 |
| --- | --- |
| `GATEWAY_OPENAI_WS_DUAL_LEG_ENABLED` | 兼容账号自动进入逐轮双链路控制器 |
| `GATEWAY_OPENAI_WS_GPT_THRESHOLD_ROUTING_ENABLED` | 最终 GPT 模型阈值判定直接生效 |
| `GATEWAY_OPENAI_WS_MODEL_SWITCH_FULL_RETRY_ENABLED` | 兼容客户端直接使用受控全量重连 |
| `GATEWAY_OPENAI_WS_MODEL_SWITCH_ALLOWED_GROUP_IDS` | 不再需要分组灰度名单，仍执行正常分组权限校验 |
| `GATEWAY_OPENAI_WS_MODEL_SWITCH_ALLOWED_CLIENT_VERSIONS` | 改为内置已验证客户端兼容性检查 |
| `GATEWAY_OPENAI_WS_DUAL_LEG_MEMORY_BUDGET_BYTES` | 删除进程内存预留与预算拒绝机制 |
| `GATEWAY_OPENAI_WS_DUAL_LEG_ACCOUNT_MEMORY_BUDGET_BYTES` | 删除账号内存预留与预算拒绝机制 |

代码不再读取这些字段；即使旧 Compose/.env 中还存在也不影响 beta2，无需再执行 beta1 配置脚本。为保证回滚可靠，发布脚本不擅自删改服务器旧配置；回滚 beta1 后旧配置仍按 beta1 语义生效。

保留此前已有的 `GATEWAY_OPENAI_WS_ENABLED`、`MODE_ROUTER_V2_ENABLED`、`RESPONSES_WEBSOCKETS_V2`、`FORCE_HTTP`、账号 mode、协议档案、单帧大小、超时、账号连接数及并发限制。不移除管理员设置的 Docker 内存限制。既有 OpenAI 显式 passthrough/ctx_pool/http_bridge/off 保持其模式语义，不强制改写成 adaptive；adaptive 账号与原生供应商 HTTP 兼容账号无需新开关即可走双链路。默认生效不等于绕过原有全局禁用、强制 HTTP 或账号能力校验。

## 使用与限制

- 备用机基础 WS 和 mode router v2 已开启，升级 beta2 后不用配置新增变量。
- DeepSeek/Kimi 等账号必须真正支持 HTTP Responses（Responses 或已验证 Adaptive），方可客户端 WS、上游 HTTP。仅有 Chat Completions 的账号仍不支持此链路。
- 有明确 Responses WS v2 能力的供应商仍需正确 WS 档案和 URL，不猜测端点。
- 恢复限于 Codex `codex_cli_rs/0.155.0-alpha.16.4` 和同系列桌面 UA `codex_cli_rs/0.155.0`。其他客户端不自动重放。严格 user/API key/group owner、Redis CAS、90 秒迁移意图、120 秒去重、冻结目标和 NOT_SENT 条件不变。
- SENT_UNKNOWN 后禁止自动跨账号重发，返回 `upstream_execution_unknown`，保留已观察 usage。
- 不再计算八倍工作集、JSON 节点预算、输入/输出/history 预留或内存拒绝。原有单帧大小限制继续生效（默认 64 MiB，可使用既有配置调整）；历史独立持有字节，避免小切片钉住整帧。
- 无共享预留意味着并发大帧可能推高 RSS，甚至触发容器 OOM。16 MiB 合成测试仅验证请求处理和连接释放，不是容量承诺。容量依赖原有并发、帧大小、容器限制及实际压测。

## 验证

覆盖无新增配置下 HTTP/WS 多轮交替、原生供应商准入、GPT/非 GPT 阈值、全量/带 ID 切模型恢复、提交不明不重放、严格 owner/Redis CAS、连接数并发限制与幂等释放、单帧限制与大帧、历史独立持有。前端和使用记录跑回归，真实 Codex CLI、PostgreSQL、Linux/amd64 容器探针随新包执行并保存日志。

## 发布与回滚

仅备用机 `cc.thkl.site`。从干净发布分支打包；回滚基线是当前 beta1 提交 `3fdfb9bb427eb6d23a17aa471c6c3c15e2821a2b`，不是 v0.2.9。

无新增/修改数据库迁移，要求部署和回滚前后迁移列表及摘要一致。备份 Compose、.env、旧镜像与数据库后，停止应用并等待迁移窗口排空，再切镜像；失败恢复 beta1，保留业务数据及既有 ingress_transport 列。

脚本位于 `tools/releases/dual-leg-default-v028-beta2/`。上传不等于部署，先运行包内预检，再执行 deploy.sh；回滚执行同包 rollback.sh。不要把 beta1 配置工具用于 beta2。
