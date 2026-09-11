# 自适应 WS / HTTP 模式

## 使用与行为

基于 `codex/release-v024-memory-ttft`（`257eee18e`），开发分支为 `codex/ws-adaptive-http-bridge`。本次只实现和本地验证，没有部署或连接生产服务器。

在 OpenAI 账号的 WS mode 中选择 **自适应 WS / HTTP（adaptive）**，支持新增、编辑和批量编辑，以及 OAuth/API Key 账号。全局 `gateway.openai_ws.mode_router_v2_enabled` 必须开启；还需原有账号类型 WS 能力开启。默认模式不变。固定 `http_bridge`、Grok、插件强制 HTTP 路径不会因此自动切成原生 WS。

- 完整首帧低于默认 15 MiB 阈值：进入 passthrough，不额外维护历史缓存。
- 达到阈值：保持客户端 WS，以完整输入初始化桥接历史，逐轮把增量和已知输出合并成完整 HTTP 请求。
- 压缩后的完整输入替换旧历史；实际准备发送的完整 WS 请求低于阈值且工具上下文完整时，使用新的上游 WS 启动该轮。
- 切换成功后释放桥接历史及迁移引用，后续持续 passthrough。每条客户端连接只支持一次 HTTP→WS 切换；不缓存历史以支持反向切换。
- 未知 `previous_response_id` 明确失败，不静默删除后发送残缺输入。passthrough 后单个超大增量、上游断开或丢失状态沿用错误/客户端重连恢复路径。

阈值沿用 `gateway.openai_ws.http_bridge_threshold_bytes`，默认 `15 * 1024 * 1024`，相等时使用 HTTP。显式 adaptive 不依赖旧路径的 `http_bridge_enabled` 自动桥接开关。

## 大消息接收与资源生命周期

adaptive 的客户端 WS 读取使用 `SetReadLimit(-1)`，首帧和后续消息都不受原有 64 MiB 字节上限限制，也没有新增固定的上下文缓存字节预算。超时、连接准入、取消和上游自身限制仍然有效。

首帧包含选号所需模型，因此握手前先检查同一调度账号库存是否存在可用 adaptive 路由。有可能进入该模式时允许大首帧；最终选择非 adaptive 账号后补做原字节上限校验，并恢复后续读取上限。纯非 adaptive 路由维持原接收限制。

单一读取器贯穿 HTTP 和 WS 执行器，拒绝重叠的 `response.create`，检测客户端断开，并在账号重试之间保留连接。adaptive 的上游 HTTP 请求保留会话取消信号，避免断开后继续占用大请求资源；旧桥接路径维持原取消策略。

桥接使用原始 JSON 视图，adaptive 的账号身份来源正文借用不可变输入，避免一份完整正文复制。通过返回交接对象退出旧桥接函数，再进入长期 WS relay，避免旧栈和闭包保活压缩前的历史。逐轮钩子偏移编号，保持定价、审核、模型映射和使用记录连续。

## 请求记录与回滚

数据库只新增 `usage_logs.upstream_transport SMALLINT NOT NULL DEFAULT 0`：

| 值 | 含义 |
|---|---|
| 0 | 未知，包括历史记录和未标注路径 |
| 1 | 实际通过 HTTP 请求上游 |
| 2 | 实际通过 WebSocket 请求上游 |

继续使用已有 `request_type`，不添加数值 6，不修改原请求类型约束。OpenAI 转发结果逐轮标注实际协议；固定 HTTP bridge 同样标注为 1。`ws_v2 + upstream_transport=1` 展示为 **WS → HTTP**，其余 WS 记录继续展示 WS；cyber 等特殊分类保留优先级。

`ws_http` 仅为前端派生标签和筛选选项，不作为数据库或 API 的新请求类型写入。前端筛选转换为 `request_type=ws_v2&upstream_transport=1`，列表、统计、图表及导出使用相同条件。服务端接受可选 `upstream_transport=0|1|2`，统计缓存键包含该字段。

迁移为 `238_usage_log_upstream_transport.sql`。旧版本回滚后可以忽略新增列，旧写入默认填 0，无需删除新字段或清理记录。账号模式配置是独立事项：回滚到不认识 adaptive 的版本前，建议将对应账号设置成旧版支持的 passthrough 或 http_bridge；旧代码对未知模式的默认处理不等价于 adaptive。

## 本地验证与测量

使用本地真实 WebSocket 连接和模拟 HTTP/WS 上游，覆盖大首帧、65 MiB 分片消息、压缩消息、后续大消息、HTTP→WS 切换、输出历史、未知响应引用、重叠请求、取消、阈值边界、模式配置及用量字段。数据库写入/扫描/筛选使用仓库现有测试与 SQL mock；没有连接真实生产数据库或执行生产迁移，也没有使用真实上游账号做端到端验证。

以下为单次诊断样本，整个进程同时包含本地客户端与服务端，峰值由 2 ms 采样获得，存活堆在显式 GC 后读取。不能将它直接当作生产 RSS 或容量承诺。65 MiB 的旧路径仅在诊断中将接收上限调到 80 MiB，以便比较桥接开销；产品中的旧模式默认仍为 64 MiB。

| 场景 | 模式 | 峰值堆增量 MiB | 压缩后存活堆增量 MiB | 首轮墙钟 ms | 首轮上游 TTFT ms |
|---|---|---:|---:|---:|---:|
| small | passthrough | 0.93 | 0.87 | 2 | 0 |
| small | adaptive | 1.17 | 1.01 | 2 | 0 |
| history | passthrough | 184.22 | 21.87 | 707 | 46 |
| history | adaptive | 140.21 | 1.72 | 684 | 47 |
| images | passthrough | 221.11 | 21.87 | 607 | 47 |
| images | adaptive | 181.10 | 1.82 | 580 | 46 |
| large65 | passthrough | 524.24 | 66.86 | 2289 | 149 |
| large65 | adaptive | 455.22 | 1.82 | 2218 | 154 |

首轮墙钟包括客户端写入、读取、解析和模拟上游处理；上游 TTFT 是现有逐轮统计口径。小请求和切换轮的模拟 TTFT 为 0 ms，不代表真实网络或模型延迟。20 MiB 历史、图片和 65 MiB 场景切换后均验证压缩前的大历史不再保活。大消息接收和解析仍有显著峰值内存开销。

结构化原始结果位于 `tools/diagnostics/adaptive-ws/memory-results.json`，验证输出保存在同目录日志中。复现命令：

```sh
# 在 backend 中，使用 Go 1.27。
go test ./internal/service ./internal/handler -run 'TestAdaptive|TestOpenAIWSHTTPBridge|TestPassthroughIngress' -count=1
go test -race -short ./internal/service ./internal/handler -run 'TestAdaptive|TestPassthroughIngress|TestOpenAIWSHTTPBridgeSessionIsolation' -count=1
go test ./internal/repository ./internal/config ./migrations ./internal/handler/dto ./internal/handler/admin -count=1
SUB2API_ADAPTIVE_PROFILE=1 go test ./internal/service -run '^TestAdaptiveMemoryProfile$' -count=1 -v

# 在 frontend 中，使用兼容项目的 Node 与 pnpm 版本。
pnpm install --frozen-lockfile
pnpm exec vue-tsc --noEmit
pnpm exec vitest run src/utils/__tests__/adaptiveWs.spec.ts src/utils/__tests__/openaiWsMode.spec.ts src/i18n/__tests__/wsModeLocaleDesc.spec.ts src/i18n/__tests__/localeKeyCompleteness.spec.ts src/components/account/__tests__/BulkEditAccountModal.spec.ts src/components/account/__tests__/EditAccountModal.spec.ts src/components/account/__tests__/CreateAccountModal.spec.ts src/components/admin/usage/__tests__/UsageTable.spec.ts src/api
```

## 验证结果摘要

- 前端：31 个测试文件、363 项测试通过；Vue TypeScript 检查通过。
- 后端：相关 ingress/桥接/passthrough 回归通过；repository、config、migrations、DTO 和 admin 包全量单元测试通过。
- race：包含大于 64 MiB 消息的 adaptive/handler 测试通过；补充的桥接取消、阈值边界及旧 HTTP bridge 回归通过。
- 新模式压缩后存活堆增量在大请求样本中约 1.7～1.8 MiB；65 MiB 样本峰值堆增量约 455 MiB，旧路径约 524 MiB，均包含测试客户端。
