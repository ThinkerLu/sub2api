# Responses WS 逐轮双链路与共享内存准入

目标版本：`v0.2.10-dual-leg-thinking-ws`（待发布）。开发基线为 `326a74ac3`，关联本分支 `feat(ws): route adaptive turns with bounded shared reservations` 提交。

## 目的和使用

在账号允许的 WS 候选模式及 HTTP-only 模式下保持客户端 Responses WS，逐个 turn 选择上游 HTTP/SSE 或 WS。启用条件为 `gateway.openai_ws.mode_router_v2_enabled=true`、`dual_leg_enabled=true`，并显式设置正数 `dual_leg_memory_budget_bytes` 和不超过总额的 `dual_leg_account_memory_budget_bytes`。默认关闭，内存额度默认 0，未配置额度时不能启用。

双链路自动使用最终 GPT 模型阈值判定。模型在明确名单中且原始、改写、replay 候选体任一达到 `http_bridge_threshold_bytes` 时走 HTTP；其他模型不套此选路门槛。所有模型仍受 `client_read_limit_bytes`、共享预算及上游能力约束。

## 行为和限制

- 每个 turn 只运行一个执行器。完成后返回循环；HTTP→WS→HTTP→WS 保持客户端连接，切入 HTTP 前关闭旧 WS socket，HTTP 不继承 WS turn-state。
- 同一账号、同一有效 WS 可以保留增量旧 ID。切换连接或 HTTP/WS 后使用已验证的 bridge 输入与输出历史恢复全量请求，未知旧 ID 或缺工具上下文拒绝。
- 握手失败且请求 `NOT_SENT` 时可向同一账号 HTTP 回退。开始 WS 写入或 HTTP 提交前标记 `SENT_UNKNOWN`，之后禁止内部自动重放和账号 failover。普通执行故障返回 `upstream_execution_unknown`（非缓存未命中），保留客户端连接；下一 turn 必须给出新的全量上下文。
- 输入按 32 KiB 分块增长预留，改写、replay、上游解码窗口、保留输出均计入同一实例/账号预算。拒绝不切到 HTTP；关闭与重复释放安全。历史预算保留到被新窗口替换或连接关闭。
- 当前估算使用 8 倍工作集系数并预留结构与 socket 开销；已通过本地 16 MiB 并发预算拒绝及容器试验。预留额度不是进程 RSS 硬上限，不承诺集群容量，生产启用前仍需结合进程基线、容器限制与真实负载校准。
- 跨账号恢复、目标历史净化、共享迁移意图、协议档案和入站传输由同版本独立功能提供，适用边界与客户端限制分别见对应文档。

## 配置和数据

新增上述三个 `gateway.openai_ws` 配置字段，无数据库迁移，不更改客户端公开协议。已有 HTTP/WS 流量计数和每轮计费回调复用；握手失败回退不增加第二次客户计费回调。旧模式在关闭 `dual_leg_enabled` 时保持原流程。

## 验证

`go test ./internal/service -run '^TestDualLeg|^TestWSMemory' -count=1` 覆盖四轮交替、输出历史恢复、旧 socket 关闭、握手失败安全回退、HTTP 提交不明不重放、下一全量 turn 继续、预算拒绝零上游、共享总额/账号额度、并发增长与幂等释放。竞态验证使用相同范围的 `go test -race`。另外运行完整 service 回归、WS handler 测试和配置测试。

## 发布与回滚

仅在整体方案验收后合入干净发布分支并构建备用机发布包。发布包必须保留默认关闭状态，实际灰度开启须有独立压测与客户端验收记录。回滚关闭双链路配置并恢复上一应用镜像；本批不迁移业务数据，旧响应归属不改绑。过程中的失败 turn 不得通过回滚脚本自动重发。
