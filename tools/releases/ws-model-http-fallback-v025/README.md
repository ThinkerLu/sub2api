# v0.2.5 模型 HTTP 即时回退发布

基于官方 v0.2.5 合并 adaptive WS/HTTP 分支，包含首消息空闲超时续期、官方连接池修复和模型级传输预检。

当 Codex WS 握手的 `x-codex-routing-hint` 所指模型没有任何原生 WS 账号时，网关在升级前返回 HTTP 426；Codex 会立即切换到 HTTP Responses，不再先消耗 5 次 WS 重试。没有 routing hint 时，网关在首条 `response.create` 确认模型后记录该客户端会话，下一次 WS 握手返回 426。HTTP-only、force_http、off 和 HTTP bridge 账号不会被绑定为已建立的原生 WS 会话。

## 部署与回滚

进入本发布目录后执行：

```sh
sha256sum -c SHA256SUMS
sudo python3 release.py preflight
sudo ./deploy.sh
# 需要回滚时
sudo ./rollback.sh
```

默认 Compose 目录 /home/thinkerlu/sub2api-deploy，可用 --deploy-dir 指定。仅接受实际运行基线 06ab8582809a1acb472da000ec1980db732046b5（v0.2.4-ws-adaptive-warmup-reuse）；部署前保存当前实际容器镜像和 Compose/.env。仅切换 sub2api 服务，120 秒健康检查失败会尝试自动恢复。

部署自动执行 v0.2.5 的两个新增迁移：`238_opencode_go_platform.sql` 和 `238_purge_unlimited_user_platform_quotas.sql`。回滚恢复本次部署前的 adaptive 镜像与配置，不把账号 adaptive 改成 passthrough，不删除数据库列或数据；v0.2.5 新增迁移记录作为纯增量保留。使用独立回滚指针 `.ws-model-http-fallback-v025-backup-path`。

至少需要512 MiB磁盘余量（上传后计算），以及管理员sudo权限。脚本不清理任何镜像或数据卷，不执行数据库全量备份。当前能力适配现有单实例Compose内置PostgreSQL/Redis拓扑。

## 验证

```sh
python3 -m unittest discover -s . -p test_release.py -v
```

包内附源码bundle/patch、前后端构建日志、回归/race记录、Linux镜像冒烟记录。功能测试覆盖模型传输预检、HTTP 426 握手回退、首消息按接收进度续期、adaptive 生命周期以及 v0.2.5 的连接池与执行作用域修复。

本目录为独立发布，使用专属回滚指针，不覆盖旧预热包或旧包回滚记录。部署脚本不清理旧镜像、包、数据卷或业务数据。
