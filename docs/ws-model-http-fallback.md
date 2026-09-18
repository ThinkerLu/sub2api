# v0.2.5 模型级 HTTP 即时回退

## 行为

Codex 发起 Responses WebSocket 握手时，握手头 `x-codex-routing-hint` 通常包含
`model=<model>`。网关在升级前解析该模型并应用分组渠道映射，然后检查账号池中是否
至少存在一个支持该模型的原生 Responses WebSocket 账号。

若不存在，网关直接返回 HTTP `426 Upgrade Required`。Codex 将该状态解释为
WebSocket 不可用，永久把当前会话切换为 HTTP Responses，不再执行 5 次 WS 重试。

若 Codex 未发送 routing hint，网关仍会升级连接并在首条 `response.create` 中取得
真实模型。确认没有原生 WS 路由后，网关按 API Key 与 `thread-id`、`session-id`
或 `x-client-request-id` 记录 5 分钟会话标记，并关闭当前 WS。Codex 下一次握手会
命中该标记并收到 426，因此最多多一次连接尝试。

## 账号范围

以下账号不会承接已建立的原生 WS 会话：

- 全局或账号级 `force_http`
- 全局 WS 功能关闭
- OAuth/API Key 类型 WS 开关关闭
- 账号模式 `off`
- 账号模式 `http_bridge`

`ctx_pool`、`passthrough`、`adaptive` 及兼容旧模式 `shared`、`dedicated` 仍按原生
WS 账号处理。调度器的 `responses_websockets_v2_ingress` 过滤与握手预检使用同一
protocol resolver，避免出现“预检说有 WS、实际选了 HTTP-only 账号”的分叉。

预检只判断静态能力，不判断忙碌、排队或临时限额。只要存在一个原生 WS 账号，请求
仍进入正常 WS 队列、重试和账号切换流程。

## 首消息超时

`client_first_message_timeout_seconds` 从“完整读取并解压首消息的总期限”改为
“连续无入站字节的空闲期限”。网关在 coder/websocket 帧缓冲和解压之前观察 TCP
读取，因此不完整帧、压缩数据和控制帧都会续期；持续上传可以超过 30 秒。

日志新增：

- `received_bytes`：触发超时前实际观察到的入站字节数。
- `read_idle`：最后收到数据至超时判定的空闲时长。

完整消息仍在 30 秒无数据时关闭，错误原因改为
`first response.create message idle timeout`，避免把慢上传误报为缺少首消息。

## 验证入口

```sh
cd backend
go test ./internal/service ./internal/handler -run 'Test(OpenAIWSModelRouteAvailable|OpenAIWSHTTPFallbackMarker|OpenAIWSRoutingHintModel|OpenAIResponsesWebSocket_NoNativeWSModelRouteReturnsUpgradeRequired|OpenAIResponsesWebSocket_FirstMessageTimeoutUsesConfig)$'
```
