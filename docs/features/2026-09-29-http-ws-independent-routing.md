# HTTP 入站独立选择上游 WS

## 范围与基线

开发分支：`codex/dual-leg-http-completion`，起点为发布分支 `f7e0f0f62`，官方基线 v0.2.9。本次不改应用版本，不构建发布包、不上传、不部署服务器；本地 Linux 二进制只用于编译和启动验证，没有发布版本元数据，不能分发。发布时须分配未使用的 beta 序号。本轮用户授权测试后分批提交并合入发布分支；关联提交见验收记录。

依据：工程根目录 `docs/designs/2026-09-25-dual-leg-responses-ws.md`、`docs/diagnostics/sub2api-thinking-replay-plan-2026-09-25.md`、`docs/research/codex-ws-account-switch-reconnect-study.md`。后续用户要求删除新增环境开关和内存预算，优先于原方案中相应章节；不恢复这些能力。不把尚未合入发布分支的 probe 功能纳入本次实现。

## 修复与用法

旧 `Forward` 先解析账号 WS 配置，再因 HTTP 入站将结果覆盖为 HTTP；启用自动透传时还会提前调用 HTTP 转发。本次删除入站协议覆盖，并让选中的 WS 请求进入现有 WSv2 转发器。HTTP 请求不需要新增开关；仍受原有全局 WS 配置、账号能力及模式限制。

| 客户端到 sub2api | sub2api 到上游 | 实现与记录 |
| --- | --- | --- |
| HTTP | HTTP | 原 HTTP 转发；off、http_bridge、强制 HTTP、不支持 WS 的账号继续使用；记录 1/1 |
| HTTP | WS | HTTP body 转成 response.create；上游事件转换为 SSE 或最终 JSON；记录 1/2 |
| WS | HTTP | 既有 HTTP bridge / adaptive 按轮 HTTP 路径；记录 2/1 |
| WS | WS | 既有 passthrough、ctx_pool、adaptive 保活路径；记录 2/2 |

HTTP→WS 是协议转换，不是把客户端 HTTP 连接当作 WebSocket。非 GPT 不受 GPT 专属阈值影响；最终目标 GPT 的原始 body、改写 body、带 turn metadata 的实际 response.create 帧达到阈值时，在提交前选择 HTTP。compact 端点保持 HTTP。依赖连接内状态的旧 ID 不得因阈值改发 HTTP；要求完整上下文或拒绝。HTTP 入口没有代理保存的跨轮完整 replay，不伪造完整历史。

上游写入尝试之后的失败不证明未执行。HTTP→WS 路径此时停止内部重放，不转发 HTTP。握手/取连接之前的既有重试仍保留；本次没有增加失败自动换号。请求已发出后的标准错误也保守返回客户端，不尝试丢弃 ID 重放。

2026-09-29 本机真实 Codex CLI `0.158.0-alpha.2.1` 通过标准缓存缺失错误后的完整重发、未执行时关闭后重发、执行未知不重发三项合成探针。受控切模型兼容名单加入这个精确版本；不放开 `0.158.0` 或其他未验证补丁版本。

## 2026-09-29 用户确认的选路约定

- `入站-上游` 中，阈值只决定第二段连接。客户端入站为 WS 时，降级是 WS-WS → WS-HTTP，提升是 WS-HTTP → WS-WS；不会把客户端 WS 改成 HTTP。客户端入站为 HTTP 时，对应 HTTP-WS ↔ HTTP-HTTP，HTTP 客户端不会因此升级成 WS。
- 针对最终上游 GPT 模型，当前实现阈值为 15 MiB（15 × 1024 × 1024 字节），任一受检报文大小达到阈值即候选 HTTP。受检报文包括原始帧、改写后请求及本轮恢复所需的完整 replay；不仅是客户端发来的增量帧。非 GPT 不受此 GPT 专属阈值影响。
- 正常轮间提升条件：目标账号仍允许并支持 WSv2、本轮受检大小均小于阈值、上下文可移植且工具 call/output 完整、并且本轮尚未提交。只在上一轮结束之后、本轮发送之前决定；当前增量帧变小但完整 replay 仍大时，不保证提升。
- 接受跨账号通过受控客户端全量重连，当前不要求实现“代理已有完整历史时保持客户端 WS 不断开的直接跨账号迁移”。重连应先恢复客户端 WS，以 WS-WS 为首选候选，不继承上一连接的 HTTP 降级结果；仍按新目标账号能力、全量请求大小和上下文安全规则决定是否改走 WS-HTTP。“先尝试 WS-WS”不表示先向 WS 提交已超阈值的帧，再重发 HTTP。本轮已由 TestWSModelMigrationHandlerReconnectPrefersWSAfterHTTP 验证该策略；客户端仍受已验证版本名单约束。
- 接受当前 HTTP 入站以“请求提供完整、可移植上下文”为使用前提。代理不因 HTTP 协议就断言实际 body 一定完整，不自动缓存或补造 HTTP 跨轮历史。携带依赖旧 WS 连接的 previous_response_id 且无法安全转移时，可以拒绝并要求完整上下文；不得静默丢弃旧 ID 后发送不完整增量。
- 不论哪种入站，本轮已尝试提交而执行结果不明时，不通过改协议或换账号自动重放。HTTP 入站的握手失败降级已补充，条件和验收见下节。

## HTTP 入站握手失败降级

仅对 adaptive 模式：实际 WS 拨号/握手错误证明尚未提交 response.create，并且请求未取消、未向客户端输出、同一目标账号有兼容 HTTP Responses、input 已提供、无 previous_response_id 依赖且工具上下文完整时，HTTP-WS 可降为 HTTP-HTTP。本轮只向同一账号发一次 HTTP 生成请求，不切换账号。

允许网络拨号错误及握手 HTTP 404/405/426/500/501/502/503/504；认证 401/403、限流 429、错误体明确的限额错误（即使外层 503）、连接池排队/容量失败、配置错误、generate=false 预热不走此降级。显式 ctx_pool/passthrough 模式保持原来的错误处理，不扩大 adaptive 回退范围。

每次请求重新解析 WS 能力，不写入永久 HTTP 模式。握手恢复后，下次合格 HTTP 请求重新尝试 HTTP-WS；原有账号健康冷却仍有效，不借换协议绕过健康、认证或配额政策。HTTP 回退一旦开始提交，内部字段修正重试、执行不明错误引起的 handler 换账号均被阻止；错误返回 upstream_execution_unknown，观测到的部分用量结果仍保留。尚未写出下游响应时，执行未知返回 HTTP 400 和 application/json，避免 HTTP SDK 将 502 当作可重试故障；HTTP→WS 写入/读取失败同样适用。真实 Codex HTTP 探针验证只发送一次请求。

WS 入站也验证了同一客户端连接的 WS 握手失败 → WS-HTTP → 后续 WS-WS；回到 WS 时使用已恢复的完整历史，不把 HTTP response ID 直接发往新 WS。跨账号受控全量重连测试验证上一连接为 WS-HTTP 时，新连接可按新目标能力选择 WS-WS，且重复认领不产生第二次账单。

## 工具链说明

`tools/install-go.sh` 按 go.mod 版本和机器架构下载并核对官方 SHA256，在 `$HOME/.local/share/go/<version>` 安装。`tools/go.sh` 固定 GOROOT/GOTOOLCHAIN，关键标准库缺失时失败。已安装 go1.27.0 darwin-arm64，并通过 bytes/archive/tar 测试。当前 v0.2.9 主机组包脚本默认调用该入口；历史发布脚本不追改归档，必须用 SUB2API_GO 显式指定持久工具链。

## 验收对应表

| 设计项 | 自动化验证 |
| --- | --- |
| HTTP→WS 的真实连接、JSON/SSE、HTTP 自动透传开关 | TestHTTPIngressIndependentWebSocketUpstream |
| GPT 与第三方思考正文在实际 WS 帧中分别清理/保留 | 同上，检查上游收到的 frame |
| HTTP→WS 用量入站/上游维度 | 同上，将真实 Forward 结果交给 RecordUsage 并检查仓储参数 |
| 实际 Responses HTTP handler、账号选择与异步记账 | TestHTTPWSHandlerRecordsActualTransports，测试身份中间件、本地 WS 上游；JSON/SSE 两种返回均检查实际用量字段 |
| HTTP 握手失败后降级、下一请求升回 WS、同账号一次记账 | TestHTTPWSHandlerHandshakeFallbackAndNextRequestUpgrade |
| 降级准入与回退 HTTP 失败不重放/不换号 | TestHTTPWSHandshakeFallbackAdmission、TestHTTPWSFallbackHTTPFailureCannotReplayOrFailover |
| WS 握手恢复后同客户端连接升回 WS | TestDualLegHandshakeRecoveryReattemptsWS |
| 跨账号全量重连不继承上一连接的 HTTP 选择 | TestWSModelMigrationHandlerReconnectPrefersWSAfterHTTP |
| HTTP 模式约束、模型映射和 GPT 阈值 | TestHTTPIngressHonorsHTTPRouteConstraints |
| 写入尝试失败禁止内部重放 | TestHTTPIngressWriteFailureDoesNotReplay |
| 实际 WS 帧因 metadata 增大，提交前转 HTTP；连接内旧 ID 不跨协议 | TestHTTPIngressFrameThresholdBeforeUpstreamSubmission |
| WS 入站逐轮 HTTP/WS 切换、未知执行禁止 failover | TestDualLegAlternatesUpstreamAtTurnBoundaries、TestDualLegHTTPUnknownSubmissionCannotFailover |
| 同账号换协议重建历史、原历史不污染 | TestDualLegSameAccountProtocolSwitchRebuildsPortableHistory 及目标 thinking 测试 |
| 归属绑定、单次认领、缺工具/托管状态拒绝 | TestWSRecoveryOwnerRequiresExactUserKeyAndGroup、TestWSMigrationIntentSingleClaimAndFailClosed、TestWSMigrationHistoryRejectsMissingToolsAndHostedState |
| 客户端 full retry 与未知执行不重试 | TestCodexWSRecoveryProbe、TestCodexWSUnknownExecutionDoesNotRetry、TestCodexWSPrecommitCloseRetriesFullContext（显式指定本机 CLI，合成上游） |
| HTTP 客户端执行未知不自动重试 | TestCodexHTTPUnknownExecutionDoesNotRetry（真实 CLI，隔离配置和合成凭据） |
| 原 WS passthrough 轮间保活和终态先于 EOF | TestAdaptiveRetainedUpstreamAnswersIdlePing、TestAdaptiveRetainedReaderDeliversTerminalBeforePeerEOF |
| 非 OpenAI 显式协议档案及无档案 HTTP | openai_ws_protocol_profile_test.go |
| 入站列迁移、双维度数据库读写、旧行兼容 | TestUsageIngressTransportPostgresRoundTrip（隔离 PostgreSQL 容器） |
| 页面标签、过滤、导出与历史兼容 | 前端 adaptiveWs.spec.ts、usageRequestType 相关测试及后端 handler/repository 测试 |

运行记录见同目录 `2026-09-29-dual-leg-verification.md`；不能以测试名称存在代替通过，也不能将未启用的可选探针记为通过。

## 配置、迁移与发布回滚

无新环境开关、数据库迁移或 HTTP API 参数。已有 ingress_transport 迁移沿用。仅修改本地代码和 AGENTS.md，不读取生产凭据或发起真实付费模型请求。回滚使用各目标发布包脚本，恢复旧应用会恢复 HTTP 入站强制 HTTP 的旧行为；保留业务数据。生产模型、真实账号配额/错误和五分钟空闲验收须在目标明确并部署后另验，不以本地合成上游测试代替。

跨账号仍采用已实现且验证过的受控客户端全量重连，用户已接受该取舍；免客户端重连的直接跨账号迁移不列为当前必须补齐项。可见思考转普通助手文本是原方案可选项，本轮也不启用。
