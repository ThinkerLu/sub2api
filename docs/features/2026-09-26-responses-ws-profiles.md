# 显式 Responses WS 协议档案

> beta2 更新：新增环境开关、恢复灰度配置和内存预算已移除；以下为 beta1 历史设计。当前行为见 [双链路默认生效](2026-09-26-dual-leg-default-no-budget.md)。

目标版本：`v0.2.8-beta1-dual-leg-thinking-ws`（待发布）。关联本分支的账号 WS 档案与生命周期保护提交。

## 配置和行为

创建/编辑 Kimi、DeepSeek、智谱、MiniMax 和 OpenCode Go API Key 账号时，可保存显式 `responses_ws_mode`（`off`、`http_bridge`、`adaptive`），以及可选 `responses_ws_profile=openai_responses_v2` 和完整 `responses_ws_url`。默认 mode 为 off、档案为空，不继承 OpenAI 全局 ctx_pool 默认值。既有 OpenAI OAuth/API Key 专属字段及 enabled 兼容规则保持不变。

新增档案要求管理员明确选择支持 Bearer 认证、Responses WS v2 JSON 事件与 continuation 语义的端点，并配置 HTTP Responses base URL。WS endpoint 必须为 ws/wss、不可含用户信息、查询参数或 fragment，仍受既有 URL 安全策略约束。未配置有效档案不猜测 WS URL；一次握手失败不会永久改写账号能力。

启用 `dual_leg_enabled` 和有效内存预算后，原生 Responses HTTP 账号可保持客户端 WS、上游仅 HTTP。非 OpenAI WS 档案还要求 Responses/Adaptive 协议兼容；只有 Chat Completions 且未配置兼容档案的账号仍使用原有 HTTP 客户端路径。该配置不声称供应商官方端点天然支持 WS，也不改变普通 HTTP 请求的平台默认协议。

双链路开启时，现有 OpenAI WS 候选 mode 统一进入逐 turn 控制器，off/http_bridge 只候选 HTTP。跨平台模型切换通过 composite 分组既有模型归属规则确定目标平台，迁移意图同时冻结平台与账号。已验证 GPT→原生 DeepSeek HTTP→GPT，拒绝帧不计费，三次实际请求对应三个用量记录。

## 生命周期和资源

- 保留的 WS socket 计入账号连接上限（沿用硬上限及并发系数）和共享内存预算，关闭/转 HTTP 时原子释放计数与预留；HTTP transport 不占 WS socket 名额。
- 握手的 endpoint、proxy、模型、tier、认证与请求头摘要用于复用判断。兼容性变化只在 NOT_SENT 时关闭旧连接，并用完整历史向新 WS 提交一次。
- 读帧按块预留内容与 JSON 结构开销，最大嵌套深度 128；保存的输入项拥有独立字节，不以小切片保留整块大帧。原始帧空白也计入 GPT 大小判据。
- 已提交错误不会把上游 `previous_response_not_found` 原样用作客户端恢复信号；保留观测 usage，返回不可自动恢复的 `upstream_execution_unknown`。真实 Codex 探针确认此错误没有自动重连/重发。
- 每轮生成逻辑 turn ID；迁移重试沿用意图中的 ID。只有缺少上游 response ID 的已观测用量使用 `ws_turn:<id>` 作为客户账单去重键，不伪造上游 response ID。同一 response ID 出现冲突归属时保存原始审计内容并标记 ambiguous，后续迁移拒绝。

## 验证、发布和回滚

账号创建/编辑与语言键 101 项前端测试、类型检查，档案/URL/认证/HTTP-only/非 GPT 阈值、跨平台 handler、内存 socket 计数和竞态测试已覆盖。16 MiB 并发压力用例证明首个非 GPT 请求进入 WS、第二个因共享预算被拒绝，释放后额度归零。Linux/amd64 模拟容器开发快照峰值 RSS 约 266 MiB，限额 1 GiB、预留预算 512 MiB；最终包重新运行并在 manifest 记录结果。这不是生产容量承诺。

无新增数据库迁移，账号配置存储在既有 extra。默认不开启新路由；发布需与入站传输 additive 迁移和恢复状态一并验证。回滚保留 extra 和 Redis 审计记录，旧版忽略新字段；停止新 WS 并排空两分钟迁移窗口后恢复旧应用。后续容量配置必须保留进程基线与安全余量。

关联实现提交：`89c098eb6`。
