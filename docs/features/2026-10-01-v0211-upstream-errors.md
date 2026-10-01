# v0.2.11 整合与 WS 上游错误透传

目标版本：`v0.2.11-beta1-upstream-errors`（已合入发布分支，未发布）。

## 目的和来源

从发布分支 `codex/release-sub2api` 的 `c4586c46a` 创建独立分支
`codex/v0211-upstream-errors`，整合上游正式 tag `v0.2.11`，并停止把明确的
上游 WS/SSE 错误统一改写成 `upstream_execution_unknown`。

- 上游：`https://github.com/Wei-Shaw/sub2api.git`。
- tag 对象：`881a722349105e21aaa38e11de90dd05046ebe4a`。
- tag 对应提交：`96f4c115c9749078f90cbf210a01d39baf3f53b6`。
- tag 的源码 tree：`0e35899fa37140d852c17b9d4bd2558cf29e7c17`。
- 三方整合基线：v0.2.9 的 tree `212ecc21a885afd84eb9ff70df2bf5d543b864eb`；
  与既有精确快照 `f83104df6` 的 tree 相同。该快照并非正式上游提交，不能直接
  用普通共同祖先代替此次源码基线。
- 功能提交见 `codex/v0211-upstream-errors` 分支历史；发布分支合并提交见
  `codex/release-sub2api` 分支历史。

上游 tag 的 `backend/cmd/server/VERSION` 内容仍为 `0.2.10`，与 tag 名称不同。
本分支明确使用上述自定义 beta 版本，不把定制程序标记为上游正式版。

## 使用方式与行为

无需新增开关。WS 入站的 adaptive、passthrough、连接池及 HTTP/SSE 桥接路径
收到上游 `error` 或 `response.failed` 时，保留原始错误码、类型和说明。
`websocket_connection_limit_reached`、`previous_response_not_found`、限流和容量降载
错误不再因为已开始发送请求就统一变成执行未知；WS 入站路径也不再把
`server_is_overloaded`、`slow_down` 改成 `server_error`。

HTTP/SSE 转为 WS 仍需提供协议事件格式；已有客户端模型/工具名映射仍保留。
错误透传不是保证整个 SSE 字节流与上游逐字相同。普通 HTTP 入站现有的容量降载
兼容处理不在此次 WS 入站调整范围内。

代理不会因为这次透传改动自行重放失败请求。客户端能收到原始恢复信号，但是否
自动重连或重发由客户端决定；不能保证所有错误都可恢复。上游断连且没有错误或
完成事件时，仍允许代理生成 `upstream_execution_unknown`，这不属于改写上游错误。

双链路大小选路、未发送请求时的握手降级、用量观测及账号故障处理保留。
上游新增 `gpt-6.1-sol` 的模型声明、Codex 元数据、推理参数、价格和前端支持；
同时将其补入定制 GPT WS 阈值名单，原始、改写或重建请求达到阈值时可选择 HTTP。

## 合并冲突处理

- 保留定制的逐轮合成路由解析、跨模型恢复与流量字节统计。
- 接入上游新在途余额预留、日志模式的风控允许名单和管理后台能力。
- 上游合成路由测试改为使用既有共享解析器；未解析出的模型仍拒绝。
  同平台已解析模型的切换不采用上游新增的“一律重连”规则，保留定制行为。
- 测试辅助代码同时支持定制配置回调和上游风控配置覆盖。

## 配置、API 与数据迁移

本次错误透传没有新增配置、API 或数据库迁移。v0.2.9 到 v0.2.11 的上游差异
未新增 SQL 迁移文件，但包含在途余额预留等运行行为及配置变更，不能只按错误透传
补丁评估发布风险。Go 仍为 `1.27.0`，前端依赖清单和 lockfile 未变化。

## 验证方法

```sh
cd backend
../tools/go.sh test ./...
../tools/go.sh test -race ./internal/service \
  -run '^TestDualLeg|^TestAdaptiveWarmupFailure|^TestGPT61SolUsesDualLegSizeThreshold$' -count=1
cd ../frontend
pnpm typecheck
pnpm test:run
pnpm build
```

`TestDualLegUpstreamErrorsPassThrough` 覆盖两种上游传输、连接到期、缓存未命中、限流、
容量降载、失败事件及代理不二次提交。测试在移除错误改写前失败，修改后应通过。
保留断连执行不明不重放、HTTP/WS 交替、握手降级及用量结算测试。

2026-10-01 本机验证结果：

- 后端 `go test ./...` 全部通过；定向 `-race` 通过。
- 前端 337 个测试文件、2,580 个测试通过；类型检查和 Vite 构建通过。
- `go build -tags embed ./cmd/server` 通过；候选 `--version` 返回
  `0.2.11-beta1-upstream-errors`。这是本机验证程序，不是发布包。
- 配置 `SUB2API_CODEX_PROBE_BIN` 后，`TestCodexWSRecoveryProbe` 通过：本机
  CLI 连接本地模拟上游，在原始缓存未命中错误后建立第二条连接，携带全量上下文恢复。
  不使用真实 API key，不访问真实上游；与双链路透传回归测试分别验证客户端和代理。
- 本机默认 Node 16 无法启动 Vite；本次使用 Codex 配套 Node 执行前端验证，
  未修改系统 Node、依赖清单或 lockfile。重跑时应使用支持当前 Vite 的 Node。

真实上游与主服务器验收尚未执行。正式部署后应验证原始错误可见、客户端重新连接
及长连接到期恢复，不能用本地模拟测试声称真实 60 分钟场景已修复。

## 发布与回滚

本次已从开发分支合入发布分支，尚未推送、打包或部署。未来发布需在发布分支
完成完整发布包校验，独立选择目标机与回滚基线。
回滚恢复旧应用，不恢复或删除业务数据；旧版会重新启用对应错误改写行为。
