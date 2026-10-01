# probe18/19/20 备用机分支迁入

## 来源与边界

本次开发分支为 `codex/cc-probe18-20-import`，从备用机发布分支
`codex/release-sub2api-cc` 创建，正式基线为 `60a84dbbde10c855330105d5de64ea2043a945c0`。
仅向备用机发布分支迁入，不向正式发布分支反向合并，不推送、不构建、不部署。
旧开发 worktree 的未提交修改、历史发布包和验收记录保留。

| 来源 | 迁入方式 | 迁入提交 |
| --- | --- | --- |
| probe18 原生 WS 命名空间替换 | 合并 `codex/codex-v2-native-ws` 的 `f968cc1d26be9b81921d40d4156c70168d6fd075` | `92514e552` |
| probe19 HTTP→WS 流量修复 | 从完整包冻结源码选取相对 probe18 的差异 | `7397eacb8` |
| probe20 首帧失败诊断 | 从完整包冻结源码选取相对 probe19 的差异 | `53ec5c3a8` |

probe19 冻结清单 SHA256：`d7dba80b459ffdafed853a8caf328a7f896f545bcfb99cb996bad82a6ab7423f`。
probe20 冻结清单 SHA256：`5ba6aa2decb613dd75130c5472660aa00b7ad582d23945ce79a173360f5c0b73`。
本机清单与备用机发布包的校验文件一致。导入工具校验归档、构建记录、源码逐文件摘要和迁移清单。
最终 backend/frontend 源码与 probe20 冻结清单一致；生成的 `backend/internal/web/dist/` 不导入 Git，
后续打包须重新构建资源。SQL 迁移不变，本次不触发数据库升级。

## 验证记录

本轮使用持久 Go 1.27.0，通过 `tools/go.sh` 执行。
probe19 定向回归通过；probe20 的 CodexV2、WS、流量与诊断定向回归通过，相关 race 检查通过。
后端全量 `../tools/go.sh test -p 1 ./... -count=1` 通过（退出码 0），
成功日志为 `backend-full-retry.log`。合并后核对发布树与已测试开发树一致。

本机日志位于工程顶层 `tmp/cc-release-import/`，定向与 race 成功日志分别为
`probe19-tests.log`、`probe20-tests-retry.log`、`probe20-race-retry.log`。
首次并发验证因磁盘不足失败，该结果不计为通过；后续使用 `-p 1` 顺序重试。

本次没有重新进行真实上游验收，历史验收见工程顶层
`orchestration/2026-10-01-http-ws-traffic/acceptance.json` 和
`orchestration/2026-10-02-ws-first-write-diagnostics/acceptance.json`。
原始约 1.2 MB 长会话故障未复现或修复；公网域名直连 WS 的本机 DNS/TLS 路由问题仍未解决。

## 后续发布

从干净的备用机发布 worktree 构建完整二进制包，另分配新的 probe 编号。
构建时记录实际发布 HEAD，不能将本次合并提交冒称为正在运行的 probe20 程序构建来源。
部署前重新检查运行基线、配置和实际迁移；服务器现存 probe20 及其直接回退来源 probe19 不变。
只有用户认定具体功能稳定并要求整理后，才从正式发布分支另建开发分支，按功能分别整理提交。
