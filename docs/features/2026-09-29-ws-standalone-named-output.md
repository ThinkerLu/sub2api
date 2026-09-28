# WS 覆盖率判定识别具名独立输入

目标版本：待下次合并发布，本次不升版本。基线为发布分支 `codex/release-sub2api` 的 `68368526a`。

## 目的

修正 adaptive 入站的一道判定：Codex 会把外部提供的输入发成一条带 `name`、不带 `call_id` 的 `function_call_output`。HTTP 转发路径已显式保留该形态，WS 覆盖率判定却不认它，把整帧判为不可重建，随后以 1008 关闭客户端连接。客户端重试数次后自行回落到 HTTP，这一轮才会成功。

## 关键行为

新增 `isCodexStandaloneNamedFunctionOutput(itemType, callID, name)`，命中条件为：

- `type` 去空白后等于 `function_call_output`
- `call_id` 去空白后为空
- `name` 去空白后非空

`AnalyzeToolCallOutputContextCoverageBytes` 命中该条件时不再标记缺失 `call_id`，该项也不进入待配对集合。另外三种输出类型（`tool_search_output`、`custom_tool_call_output`、`mcp_tool_call_output`）不受影响。

HTTP 路径的 `sanitizeOpenAIResponsesOrphanToolOutputs` 原本内联了同一条件，现改为调用该函数。两处共用一份判定，避免再次出现偏差——本次故障的直接原因就是两处判定不一致。

未改变的行为：

- 缺 `call_id` 且 `name` 为空的输出仍按孤儿处理，adaptive 守卫仍以 1008 关闭连接
- 有 `call_id` 但帧内找不到对应调用项的输出，判定不变
- 带 `previous_response_id` 但本会话无对应历史的请求仍被拒绝

## 配置和数据

无新增配置项、无 API 变更、无数据库迁移、无外置文件。未新增逐请求日志。

## 验证

```sh
go test -tags=unit ./internal/service ./internal/service/openai_ws_v2 ./internal/handler -count=1
```

- `TestAdaptiveStandaloneNamedOutputStaysOnUpstreamWS`：具名独立输入所在的一轮走上游 WS，且不产生上游 HTTP 回退。
- `TestAdaptiveOrphanToolOutputStillClosesGuard`：真实孤儿输出仍触发 1008，确认判定未被放宽。
- `TestAnalyzeToolCallOutputContextCoverageBytes` 新增两条用例：具名独立输入判为可重建；缺 `call_id` 且 `name` 为空白仍判为不可重建。

## 发布与回滚

本改动合入发布分支，不单独构建发布包。回滚按发布分支既有方式恢复上一镜像，不涉及数据迁移。

## 来源

该判定在备用机以诊断构建（`v0.2.8-beta8`、`v0.2.8-beta9`、`v0.2.8-probe10`）复现并验证；本次只把根因修复并入发布线，诊断日志与探针发布工具留在探针分支。
