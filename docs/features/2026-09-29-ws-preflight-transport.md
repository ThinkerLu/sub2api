# 非原生 WS 上行跳过错路握手预检

目标版本：`v0.2.9-beta1-ws-standalone-named-output`。关联修复在发布分支 `codex/release-sub2api` 的 `v0.2.9` 合并之后。

## 目的

修复 Responses WebSocket 入站握手阶段的错误预检：Grok 等平台在客户端使用 WS 入站时，上行实际走 HTTP SSE。此前预检无条件按原生 `responses_websockets_v2` 上行查找账号，导致所有 Grok 账号都被判为无可用 WebSocket 路由，连接以“model has no websocket-capable account; retry over HTTP”提前关闭，账号级凭据故障转移没有机会运行。

## 关键行为

- 只有 `requiredTransport` 为 `responses_websockets_v2_ingress` 时，握手阶段才调用 `OpenAIWSModelRouteAvailable`。
- 上行选择为 HTTP SSE 时跳过原生 WS 路由预检，继续把实际传输交给账号调度器做能力和传输匹配。
- 账号选择、凭据刷新、失败切换和 HTTP bridge 的处理顺序不变。
- 不新增配置开关，不改变原生 OpenAI WS 的预检行为。

## 配置和数据

无新增配置项、无 API 变更、无数据库迁移、无数据格式变化。

## 验证

```sh
go test -tags=unit ./internal/handler -run 'TestResponsesWebSocketCredentialFailoverLoop' -count=1
go test -tags=unit ./internal/service ./internal/service/openai_ws_v2 ./internal/handler -count=1
```

重点用例覆盖 Grok WS 入站的失效凭据切换、提供方配置错误停止和父上下文取消；另由完整后端测试确认调度与网关包未回归。

## 发布与回滚

随主服务器 `v0.2.9-beta1-ws-standalone-named-output` 发布。回滚恢复上一应用镜像；无数据库迁移，业务数据保持不变。
