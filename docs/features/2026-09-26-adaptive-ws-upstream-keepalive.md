# Adaptive 上游 WS 轮间保活修复

目标版本：`v0.2.8-beta3-adaptive-ws-keepalive`。开发分支 `codex/adaptive-ws-keepalive`，应用构建与发布提交记录在 manifest；官方源码基线仍为 `v0.2.8`。

## 问题与证据

beta1 新增双链路路径、beta2 默认 adaptive 路径按 turn 调用单轮 passthrough，终止事件后结束 relay 但保留上游连接。coder/websocket 需要持续读取才能处理上游 Ping 并发送 Pong；轮间无人读取造成 `keepalive ping timeout`。下一轮复用旧连接、开始写入后才发现断开，触发 `upstream_execution_unknown`。

2026-09-26 19:16:20 的备用机日志记录了上述关闭原因。该故障发生在 GPT 的 WS→WS 路径，与 DeepSeek 账号筛选返回 426 是不同问题，不声称本修复解决所有账号能力问题。

## 实现

- 仅为 adaptive 保留的上游连接增加一个会话生命周期的常驻读取者。实际 socket 只由它读取，保留文本/二进制帧类型；原有常规 passthrough 和 ctx_pool 的生命周期不变。
- 每轮 passthrough 从单槽消息队列读取数据。轮结束或单轮读取上下文取消不会取消底层读取，上游 Ping/Pong 在轮间仍被处理。
- 已缓冲的终止/usage 帧优先于 EOF 交给当前 turn，避免上游在终止帧后断开导致计费丢失。
- 空闲期收到异常数据时关闭连接，不将其作为下一轮响应。队列不无限增长；没有新增内存预算或环境开关。
- 下一轮提交前发现连接已关闭或不可复用时，先关闭并释放连接额度，然后使用已有 NOT_SENT 全量恢复路径重建。完整输入覆盖前序用户输入、输出和工具上下文，去除旧 previous_response_id。
- 若断开与提交发生竞争，已经进入 SENT_UNKNOWN 后仍不自动重放；严格归属、去重、错误语义与 beta2 保持一致。
- 切换 HTTP、客户端断开或会话取消时关闭上游并等待读取者退出，释放账号连接额度。

## 配置、使用与边界

无需新增配置，无数据库迁移。继承 beta2 移除新增功能开关及内存预算的行为，原有 WS 开关、账号模式、协议档案、单帧和并发限制继续生效。仅支持 Chat Completions 的账号不因此获得 Responses WS 能力。

该修复消除轮间无人读取的确定性缺陷，不保证上游永不主动关闭，也不将执行结果不明视为可安全重试。发送新请求前断开的已知连接可以恢复；发送后的不确定状态仍需明确报错。

## 验证

真实本地 WS 服务器测试先在 beta2 代码上复现 `failed to ping: failed to wait for pong: context deadline exceeded`，补丁后通过：

- 第一轮结束且 relay 退出后发送上游 Ping，收到 Pong；第二轮继续复用同一连接。
- 上游在空闲期间关闭，下一轮使用新连接、全量历史、无旧 ID 完成；仅两次业务请求。
- 上游空闲时发送异常数据，连接废弃后恢复，不将旧帧送入下一轮。
- 取消一轮队列读取后，底层读取仍可接收二进制消息；会话关闭后读取者和上游退出。

针对测试及 dual-leg 测试连续三轮 `-race` 验证；完整 service、handler、relay、config 回归，以及发布构建中的前端、真实 CLI、PostgreSQL、Linux/amd64 大帧和镜像冒烟。日志随包保存在 `checks/`，其中 `keepalive-probe.log` 专门记录本次回归。

## 发布、回滚与验收

备用机 `cc.thkl.site` 已回滚至 beta1 提交 `3fdfb9bb427eb6d23a17aa471c6c3c15e2821a2b`，只读核对运行二进制摘要与健康通过。因此 beta3 脚本从 beta1 升级、也回滚至该 beta1，不使用 beta2 作为假定基线。

完整包目录由 manifest 的发布提交确定，脚本位于 `tools/releases/adaptive-ws-keepalive-v028-beta3/`。先校验 SHA256、执行 preflight，再在低流量窗口部署。脚本备份配置、旧镜像及数据库，停止应用并排空恢复窗口后切换；失败恢复旧应用，保留业务数据。预检不通过不得绕过。

部署后需用新连接完成至少两轮 GPT 请求，中间空闲超过原故障窗口，再查看同一连接的响应和保活情况。上传和本地测试不代表已完成生产部署/验收。旧 beta1 配置工具不用于 beta3。
