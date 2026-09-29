# 双链路补全验证记录

日期：2026-09-29。分支 `codex/dual-leg-http-completion`，基线 `f7e0f0f62d27b911cdee5fe4b7a6bae3b8ab3c32`。本轮用户授权补充实现、测试后提交并合入；不宣称已发布或生产验收通过。

## 环境

- Go 1.27.0 darwin-arm64，持久 GOROOT `$HOME/.local/share/go/1.27.0`，入口 `tools/go.sh`。
- pnpm 9.15.9，按冻结 lockfile 安装；未修改 lockfile。旧 pnpm 7 与 lockfile v9 不兼容，没有用 force 重写 lockfile。
- Codex CLI 0.158.0-alpha.2.1，仅使用隔离配置、合成凭据、本地假上游，不调用真实模型。
- PostgreSQL 使用既有 Colima `sub2api-build` 中的一次性容器，自动清理；不连接生产数据库。
- 日志持久目录：`$HOME/Library/Logs/sub2api/dual-leg-http-completion/`。没有将用户配置、密钥或生产数据写入日志。

## 结果

| 检查 | 命令/范围 | 结果及日志 |
| --- | --- | --- |
| Go 工具链 | tools/install-go.sh；不存在的 SUB2API_GOROOT | 标准库通过；错误目录明确返回非零；toolchain.log |
| 修复前复现 | TestHTTPIngressIndependentWebSocketUpstream | 旧实现 HTTP 入站覆盖 WS 选择，真实连接测试失败；修复后通过 |
| 全后端 unit 标签 | tools/go.sh test -p 2 -tags=unit ./... | 最终代码通过，退出码 0；backend-verified.log；服务包本轮运行 214.196 秒，其余未改包允许使用有效测试缓存。此前 count=1 的全量通过记录为 backend-unit.log；中途失败记录保留，不混作最终通过 |
| HTTP→WS 网络验证 | TestHTTPIngress* | 通过；http-ws-network.log |
| 生产 HTTP handler | TestHTTPWSHandlerRecordsActualTransports | 普通测试与 race 通过；http-ws-handler.log / http-ws-handler-race.log |
| 竞态 | HTTP ingress、DualLeg、idle keepalive、migration/owner、目标思考、协议档案 | 通过；race.log；其中 openai_ws_v2 包本次名称过滤无匹配项，不计为该包完整竞态通过 |
| 最终 HTTP 错误语义 | HTTP ingress、实际 HTTP handler、精确客户端兼容版本的 race 复跑 | 通过；final-http-race.log；连接内旧 ID 不能转 HTTP 时返回 400 full_context_required |
| 终态回调同步 | TestPassthroughIngressFollowUpCallsBeforeTurnAfterBeforeRequest，race，count=100 | 通过；callback-race-100.log |
| 真实 CLI 重连 | TestCodexWSRecoveryProbe、TestCodexWSUnknownExecutionDoesNotRetry、TestCodexWSPrecommitCloseRetriesFullContext | 三项通过；codex.log |
| 大请求 | TestAdaptiveMemoryProfile/(history\|images)/adaptive | 20 MiB 历史和图片均通过；large-payload.log；仅为本地观测，不是生产 RSS 上限承诺 |
| 超过旧 64 MiB 限制 | TestAdaptiveHandlerAcceptsFirstAndSubsequentMessagesAbove64MiB | 低并发全量回归通过；不修改原测试时限，不跳过该测试 |
| PostgreSQL | TestUsageIngressTransportPostgresRoundTrip | 通过；postgres.log |
| 前端全量 | vitest run --maxWorkers=2 --minWorkers=2 | 336 文件、2515 测试通过；frontend-tests.log |
| 前端类型 | vue-tsc --noEmit | 通过；frontend-types.log |
| 前端产物 | vite build | 通过；frontend-build.log；仍有既有 chunk 大小和 Browserslist 提示 |
| Linux amd64 | CGO_ENABLED=0 GOOS=linux GOARCH=amd64 tools/go.sh build -p 2 -tags embed -trimpath ./cmd/server | 编译成功；linux-build.log；本地验证二进制未分发 |
| Linux 启动 | 隔离容器，无网络，128 MiB，运行本次二进制 --version | 成功；linux-smoke.log；临时容器已删除，不是生产部署 |

## 本次发现的问题

1. 旧 HTTP 入站强制 HTTP 和自动透传提前返回，使普通 HTTP→WS 路由无法执行。已修复，并用实际 HTTP/WS 连接及 handler 记账验证。
2. 只检查 HTTP body 会遗漏 turn metadata 增大的最终 WS 帧。新增实际帧检查，提交前才可改选 HTTP；依赖连接内旧 ID 时拒绝。
3. HTTP→WS 原转发器存在写入/读取失败后的自动重连重放。新增写入尝试标记，无法证明未执行时停止重放，保留已有部分用量结果。
4. 带 unit 标签的三个 INSERT 参数测试依赖旧的尾部偏移；现在依据实际 INSERT 列名断言。用量接口契约补齐现有流量和传输字段，不删除生产字段来迁就旧测试。
5. 高并发编译/测试时 65 MiB 测试首次超时；降低测试并行度后原时限内通过，未放宽生产限制或修改测试时限。
6. 中断后临时日志与进程不再可用，未把未收齐结果算作通过。改用持久日志并复跑。Colima 停止导致数据库探针失败，启动既有环境后复跑通过。
7. 全量复跑暴露已有测试时序问题：接收 response.completed 不保证服务器 AfterTurn 已返回。测试现在通过 channel 等待两个回调，再精确检查次数与顺序；生产回调顺序未修改，竞态检测下重复 100 次通过。

## 边界

2026-09-29 用户确认：接受当前受控跨账号全量重连，不把免客户端重连直接迁移列为本阶段必做项；重连以 WS-WS 为首选候选，重新检查阈值与能力。HTTP 入站以全量上下文为当前使用前提，仍检查实际请求，不能假定每个 HTTP body 都完整。后续已补充 HTTP 握手失败降级与两种入站恢复 WS 的测试，见下节。

## 补充降级与提升验收

- `fallback-before.log`：真实 HTTP handler 测试在补充前失败，握手 426 被直接返回，未执行 HTTP 回退。
- `fallback-after.log` / `fallback-targeted.log`：补充后通过。普通 JSON/SSE 两种请求均先 HTTP 后 WS，同一目标账号、一次 HTTP 请求、一次 WS 生成帧、每次一次用量记录。
- `fallback-final-race.log`：新旧 HTTP 路径、模型迁移和双链路测试在 race 下重复三次通过；覆盖限额/认证/排队/旧 ID 等禁止回退条件，以及回退 HTTP 后的执行未知不重放。
- `ws-handshake-recovery.log`：WS 入站握手失败后用 HTTP 完成本轮，同一客户端 WS 的下一轮恢复 WS；检查完整历史与空旧 ID，race 三次验证。
- `fallback-full.log`：首次补充回归中，不调用本次转发代码的内容审计缓存刷新测试出现一次计数断言不稳定。保留失败记录，未修改该测试。`moderation-refresh-repeat.log` 单独连续 30 次通过；`fallback-full-final.log` 使用 `test -p 1 -tags=unit ./... -count=1` 全部通过，服务包运行 208.195 秒。合并后的复测另记 `merged-unit.log`。
- `fallback-no-replay.log`：网络错误与可触发字段修正重试的 HTTP 400 两种情况均只发一次 HTTP 请求，返回不可换号重放的错误，race 重复三次通过。
- 原有前端、PostgreSQL 和 CLI 验收仍适用，本轮未改变前端或 schema；Linux 二进制按补充后的源码重新构建并在无网络临时容器中启动，日志 `fallback-linux-build.log` / `fallback-linux-smoke.log`。临时容器已移除。
- `terminal-http-error.log`：执行未知返回 HTTP 400，真实 Codex HTTP 客户端只发送一次生成请求；JSON/SSE 请求的实际转发测试均检查状态码。`terminal-http-race.log` 对 HTTP 路径、迁移和双链路重复三次竞态通过。最终补强后的合并复测另记 `merged-final-unit.log`。

## 提交记录

- `ef00429df`：持久 Go 安装、版本入口与当前打包脚本默认工具链。
- `ec411b0c4`：四种传输组合、HTTP 握手安全降级、下一请求提升和全量重连验收。
- `690f49432`：上述两个提交合入 codex/release-sub2api；`merged-unit.log` 的全后端 unit 标签复测通过。
- `443ae8350`：执行未知 HTTP 400、JSON 错误响应与真实 HTTP 客户端不重试探针；通过 `3911e4d43` 合入发布分支。最终源码 Linux 构建与隔离启动日志为 `terminal-linux-build.log` / `terminal-linux-smoke.log`。
- `3911e4d43` 合入后的发布工作树执行 `tools/go.sh test -p 1 -tags=unit ./... -count=1` 全部通过，退出码 0，服务包 205.475 秒；完整日志 `merged-final-unit.log`。此后仅整理本文档与关联说明，不改变已验证的功能代码。
- 发布工作树原有四个模型注册表相关文件未提交差异在合并前后 SHA256 均为 `e16504e85dabfa1a37540ad03df81d7a2b1c5673d5b894e5612e23cb21828120`，未覆盖、未夹带提交。

不等同于生产服务验收；真实上游模型、长历史压缩、所有工具组合和线上五分钟空闲仍需在明确目标并部署后验证。原方案要求的内存预算与新增环境开关按用户后续要求不恢复。跨账号使用受控客户端全量重连，代理完整历史免重连跨账号执行优化未新增，可见思考转普通文本可选项未启用。参见功能文档的范围与限制。
