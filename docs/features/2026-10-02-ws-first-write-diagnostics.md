# WS 首帧写入失败诊断

首次部署：`v0.2.11-probe20-ws-first-write-diagnostics`。2026-10-02 从已部署包的冻结源码迁入 `codex/cc-probe18-20-import`，仅进入备用机发布分支，不直接进入正式发布分支。关联提交为包含本文档的诊断提交。

## 目的与行为

排查长会话 execution unknown。首帧写入失败时记录 `write_first_message_fail`，生成 unknown 事件前记录 `execution_unknown_generated`。包含账号 ID、模型、轮次 ID、消息字节数、耗时、连接复用状态、错误类型、关闭码及上下文状态。

不记录请求正文；错误文本清理换行、脱敏常见凭据并限制长度。不改变选路、重试、错误透传或客户端事件。写入失败可能已经发送部分字节，日志不能证明上游未执行。

## 使用与验证

无新增配置、API 或 SQL 迁移。按 turn_id 关联用户服务 journal 中的 `[DEBUG-ws-first-write]` 日志，完整诊断仅保存在私有记录中。

运行诊断脱敏测试、WS/CodexV2/流量定向回归和相关 race 测试；核对发布包冻结源码和迁移摘要。历史部署的 HTTP/WS 四项小请求验收通过，证据见工程顶层 `orchestration/2026-10-02-ws-first-write-diagnostics/acceptance.json`。原始约 1.2 MB 长会话故障尚未复现或修复。

## 发布与回退

在备用机发布分支干净 worktree 构建完整原生包，使用原子更新工具，不覆盖运行程序。probe20 历史直接回退来源为 probe19，不恢复数据库。此次仅迁入源码，不构建或重新部署，不改动现存回滚包。
