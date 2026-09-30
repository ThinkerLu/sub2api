# Adaptive WS 上下文累积策略

目标版本：待定，不得直接递增官方版本号。首次实现合入发布分支 `codex/release-sub2api` 的 `f5ab24d38`；手动重载与文件即开关的修订见后续合并提交。

手动重载与文件即开关修订：开发提交 `4ea62566a`，发布分支合并提交 `86db76727`。

## 目的

Adaptive WS 会为每个客户端连接保留可重建请求的完整上下文。生产流量中绝大多数后续轮次可直接透传，真正需要本地历史的比例不足 2%；保留历史却会按帧持有完整请求缓冲，抬高常驻内存。

本功能增加一个由外置文件启用的按账号策略。策略命中时，只有实际走 HTTP 桥的轮次保留上下文；纯 WS 轮次不保留。后续轮次如果需要历史但本地已经没有，服务端返回 1008 关闭客户端连接，由 Codex 携带完整上下文重连。

## 配置

配置位于 `gateway.openai_ws.context_policy`：

```yaml
gateway:
  openai_ws:
    context_policy:
      file: /app/ws-context-policy/ws-context-policy.yaml
```

对应环境变量：

```sh
GATEWAY_OPENAI_WS_CONTEXT_POLICY_FILE=/app/ws-context-policy/ws-context-policy.yaml
```

Compose 将宿主机 `./ws-context-policy` 以只读方式挂载到 `/app/ws-context-policy`。示例文件为 `deploy/ws-context-policy.example.yaml`。

策略文件格式：

```yaml
version: 1
accounts:
  account-a: {}
  account-b: {}
```

只有策略文件存在且 `accounts` 非空时策略才生效，账号名精确匹配管理后台中的 `Account.Name`。文件不存在、未配置文件路径或 `accounts` 为空映射都视为关闭，不改变现有累积行为。未知字段、缺少 `accounts`、非法版本或多 YAML 文档都会导致该次加载失败。

服务不做定时轮询。修改模型注册表或策略文件后，统一执行一次手动重载：

```sh
sudo ./reload.sh
```

该脚本向 `sub2api` 容器发送 `SIGHUP`。进程中的统一重载入口会依次重新加载模型注册表和 WS 上下文策略；一个文件失败不会阻止另一个文件重载。无效文件保留该配置各自的上一份有效快照；策略文件被删除时，策略回到关闭状态。重载只影响之后完成的轮次。

## 行为

- 策略文件不存在、账号列表为空、账号不在名单、或策略尚未初始化时，完全保持原累积行为。
- 策略命中且本轮实际走 HTTP 桥时，正常保留本地重放序列。
- 策略命中且本轮实际走 WS 时，成功轮结束后同时清空普通重放序列、账号故障转移序列以及结果对象中的对应视图；两者共享正文，只清理一份不会释放历史。
- 后续轮次出现 `previous_response_id` 但本地历史为空，或工具上下文无法重建时，沿用既有守卫返回 1008 `adaptive bridge cannot restore unknown previous_response_id` / `adaptive bridge requires complete tool context`。
- 1008 不转换为 `upstream_execution_unknown` 事件；Codex 的新连接首帧不携带 `previous_response_id`，由客户端重新发送完整上下文。
- 已写出上游且可能产生部分输出的安全性判断不变。若该阶段失败，仍按原逻辑返回未知执行事件。

## 数据与接口

无数据库迁移，无持久化数据格式变化，无公开 API 变化。新增的是进程启动配置、外置 YAML 快照和统一的手动重载入口。

## 验证

定向测试：

```sh
cd backend
../tools/go.sh test ./internal/config ./internal/service \
  -run 'OpenAIWSContextPolicy|AdaptiveContextPolicy|ReloadRuntimeConfigs|TestValidateConfig_OpenAIWSRules|TestLoadDefaultOpenAIWSConfig' \
  -count=1 -v
```

用例覆盖：

- 合法、非法、多文档和未知字段策略文件。
- 账号名单、空名单关闭、文件缺失和非命中账号保持原行为。
- 手动重载失败保留上一份有效快照；统一重载仍会继续处理其他配置。
- 策略命中时，纯 WS 成功后下一轮以 1008 结束连接，不发 `upstream_execution_unknown`。
- 策略命中时，HTTP 桥后续请求仍包含完整历史且不带 `previous_response_id`。

真实客户端探针复用 `TestCodexWSPrecommitCloseRetriesFullContext`，确认 1008 后连接数为 2、恢复成功且新连接首帧的 `previous_response_id` 为空。

## 发布与回滚

发布前先在目标机采集容器 `HeapAlloc` 基线，部署后先不放置策略文件验证无行为差异，再写入单个非关键账号并统一重载。重点观察缺历史关闭日志、客户端重连次数、用户可见错误和 `HeapAlloc`。

回滚时删除策略文件或清空 `accounts`，再执行 `sudo ./reload.sh`，即可恢复原累积行为；不需要恢复数据库。若连同镜像回滚，`ws-context-policy` 目录可以原样保留，旧版本不会使用它。
