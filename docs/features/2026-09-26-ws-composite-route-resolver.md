# Responses WS 复用 HTTP 合成路由解析器

状态：合入 `codex/release-sub2api`，等待下次合并发布。本次不递增版本、不打包、不上传、不部署。官方基线仍为 `v0.2.8`；下次发布时统一分配未使用的 beta 序号与名称。

开发分支：`codex/ws-composite-resolver`；开发基线：`b7c621d30`。关联实现提交：`951e0b497`。

## 目的与使用

Composite 分组通过 OpenAI 账号提供 `deepseek-flash` 时，HTTP 能根据账号模型归属选中 OpenAI，但 WS 首帧此前只根据模型名猜平台，误选 DeepSeek 后触发 HTTP fallback。无需更改分组或新增配置，修复后 WS 使用与 HTTP 相同的 `CompositeRouteResolver` 实例及账号归属目录。

## 行为与限制

- 首帧完成客户端模型白名单校验后，以 `responses` endpoint 调用共享解析器。显式路由优先，随后是账号模型归属，最后按原解析器的规则使用内置探测。
- 歧义归属与无法解析的模型关闭连接，不再调用模型名前缀探测来覆盖解析器的拒绝结果；目录异常与可识别模型的回退策略完全沿用 HTTP 解析器。
- 将解析出的平台用于 WS 账号筛选，将上游模型用于渠道映射、能力预检和发送；原始帧保留客户端模型，用于白名单、审计、用量与恢复身份。
- Composite 分组的握手模型提示不再提前决定能力回退，等待真实首帧；其他分组保留既有预检。仍无 WS 兼容账号时，保留原有 HTTP 回退行为。
- 第一轮转发复用账号选择时的模型映射。后续模型映射与 adaptive 切模型迁移均重新调用共享解析器，避免继承第一轮的平台。跨平台切换仍须通过已有受控重连流程；不支持迁移的连接拒绝在旧账号上直接转发。
- 逐轮审计使用当前帧的客户端模型，不把第一轮公开别名错误用于后续轮次，也不在 relay 运行期间替换共享 HTTP request。
- 不增加环境开关、内存预算、API 或数据库迁移。不改变上游原本的协议能力，不保证不支持 Responses 的账号能使用原生 WS。

## 验证

在 `backend` 目录使用 Go 1.27.0：

```sh
go test ./internal/handler ./internal/service ./internal/server/routes ./internal/service/openai_ws_v2 -run '(Composite|WebSocket|WS|ModelMigration)' -count=1
go test -race ./internal/handler ./internal/service -run '(ResponsesWebSocketComposite|OpenAIWSCompositeRoute|OpenAIWSRouteNonComposite|WSModelMigrationHandler|WSClientRequestedModel)' -count=1
go test ./cmd/server -run '^$'
```

新增测试验证：

- 本地真实 WS 握手与两轮完成，覆盖 adaptive、passthrough、dedicated。
- `deepseek-flash` 归属于 OpenAI 账号，不被名字推断改成 DeepSeek。
- 显式 Responses 别名路由优先于冲突的账号归属，首帧实际发送上游模型，第二轮改用公开模型仍正常；检查每轮使用记录。
- 错误的握手模型提示不阻止合法首帧；歧义模型以 1008 关闭且没有上游请求。
- WS 决策与 HTTP resolver 一致：真实 DeepSeek 归属、显式 endpoint 路由、歧义、目录错误、未知别名、旧上下文不会替代新解析；普通 OpenAI 分组不调用合成解析器。
- 原有切模型全量重连、跨平台 HTTP 桥接和重复恢复抑制测试。

额外启用 `-tags=unit` 的相关回归中，service、routes、relay 通过。handler 的 `TestResponsesWebSocketCredentialFailoverLoop` 三个 Grok 子用例失败：能力预检提前回退，未进入凭据切换阶段；在未修改的发布基线 `b7c621d30` 上同样复现。排除此既有测试后的相关 handler 回归单独运行。此记录不代表整个带标签测试套件全部通过。

真实双轮路由测试在收到终止事件并核对该轮使用记录后继续下一轮；并发验证覆盖这一时序。测试开发时快速连续发帧曾暴露 passthrough 前后轮回调共享状态的竞态，最终路由用例不承担该重叠时序的验收，需在单独的 relay 生命周期修复中继续核查。没有生产上游验收。

## 发布与回滚

本次仅代码和文档合入发布分支。下次发布重新确认目标服务器、当前镜像和回滚基线，按工程规则生成完整包并执行相应预检；不得把当前提交当成已部署版本。

无数据迁移，回滚只需恢复届时记录的旧镜像及配置。旧镜像将重新具有 Composite WS 模型名前缀误判问题。新版本上线后使用新连接验证实际账号、传输与至少两轮 `response.completed`；发布重启会清理进程内旧的 HTTP fallback 标记。
