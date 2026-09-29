# Adaptive WS 上下文累积策略

目标版本：待定，不得直接递增官方版本号。实现基线为发布分支 `codex/release-sub2api` 的 `e8a490a30`，开发分支为 `codex/adaptive-ws-context-policy`。

## 目的

Adaptive WS 会为每个客户端连接保留可重建请求的完整上下文。生产流量中绝大多数后续轮次可直接透传，真正需要本地历史的比例不足 2%；保留历史却会按帧持有完整请求缓冲，抬高常驻内存。

本功能增加一个默认关闭、可热重载的按账号策略。策略命中时，只有实际走 HTTP 桥的轮次保留上下文；纯 WS 轮次不保留。后续轮次如果需要历史但本地已经没有，服务端返回 1008 关闭客户端连接，由 Codex 携带完整上下文重连。

## 配置

配置位于 `gateway.openai_ws.context_policy`：

```yaml
gateway:
  openai_ws:
    context_policy:
      enabled: false
      file: /app/ws-context-policy/ws-context-policy.yaml
      reload_interval_seconds: 5
```

对应环境变量：

```sh
GATEWAY_OPENAI_WS_CONTEXT_POLICY_ENABLED=false
GATEWAY_OPENAI_WS_CONTEXT_POLICY_FILE=/app/ws-context-policy/ws-context-policy.yaml
GATEWAY_OPENAI_WS_CONTEXT_POLICY_RELOAD_INTERVAL_SECONDS=5
```

Compose 将宿主机 `./ws-context-policy` 以只读方式挂载到 `/app/ws-context-policy`。示例文件为 `deploy/ws-context-policy.example.yaml`。

策略文件格式：

```yaml
version: 1
accounts:
  account-a: {}
  account-b: {}
```

`accounts` 为空映射时策略作用于全部账号；非空时只作用于账号名精确匹配的账号。账号名取管理后台中的 `Account.Name`。未知字段、缺少 `accounts`、非法版本或多 YAML 文档都会导致该次加载失败。

修改策略文件后，服务按 `reload_interval_seconds` 轮询；也可发送 `SIGHUP` 立即重载。加载新快照或重载失败时都保留上一份有效快照，策略只影响之后完成的轮次。

## 行为

- 总开关关闭、账号不在名单、或策略尚未初始化时，完全保持原累积行为。
- 策略命中且本轮实际走 HTTP 桥时，正常保留本地重放序列。
- 策略命中且本轮实际走 WS 时，成功轮结束后同时清空普通重放序列、账号故障转移序列以及结果对象中的对应视图；两者共享正文，只清理一份不会释放历史。
- 后续轮次出现 `previous_response_id` 但本地历史为空，或工具上下文无法重建时，沿用既有守卫返回 1008 `adaptive bridge cannot restore unknown previous_response_id` / `adaptive bridge requires complete tool context`。
- 1008 不转换为 `upstream_execution_unknown` 事件；Codex 的新连接首帧不携带 `previous_response_id`，由客户端重新发送完整上下文。
- 已写出上游且可能产生部分输出的安全性判断不变。若该阶段失败，仍按原逻辑返回未知执行事件。

## 数据与接口

无数据库迁移，无持久化数据格式变化，无公开 API 变化。新增的是进程启动配置、外置 YAML 快照和周期性重载任务。

## 验证

定向测试：

```sh
cd backend
../tools/go.sh test ./internal/config ./internal/service \
  -run 'OpenAIWSContextPolicy|AdaptiveContextPolicy|TestValidateConfig_OpenAIWSRules|TestLoadDefaultOpenAIWSConfig' \
  -count=1 -v
```

用例覆盖：

- 合法、非法、多文档和未知字段策略文件。
- 账号名单、空名单全匹配、默认关闭和非命中账号保持原行为。
- 热重载失败保留上一份有效快照。
- 策略命中时，纯 WS 成功后下一轮以 1008 结束连接，不发 `upstream_execution_unknown`。
- 策略命中时，HTTP 桥后续请求仍包含完整历史且不带 `previous_response_id`。

真实客户端探针复用 `TestCodexWSPrecommitCloseRetriesFullContext`，确认 1008 后连接数为 2、恢复成功且新连接首帧的 `previous_response_id` 为空。

## 发布与回滚

发布前先在目标机采集容器 `HeapAlloc` 基线，部署后先保持开关关闭验证无行为差异，再选择单个非关键账号灰度。重点观察缺历史关闭日志、客户端重连次数、用户可见错误和 `HeapAlloc`。

回滚时先将 `GATEWAY_OPENAI_WS_CONTEXT_POLICY_ENABLED` 改为 `false` 并重载或重启，即可恢复原累积行为；不需要恢复数据库。若连同镜像回滚，策略文件可以原样保留，因为关闭状态下不会读取并生效。
