# v0.2.8-beta1 双链路 WS 与思考回传备用机发布包

完整版本为 `v0.2.8-beta1-dual-leg-thinking-ws`，官方源码基线为 `v0.2.8`（`fd80b08c90b55edcad5b00171b53f08721d30da1`）。自定义发布只递增 beta 序号，不递增官方版本号。本包替代已作废且未部署的 `v0.2.10-dual-leg-thinking-ws` 包。旧基线和回滚镜像仍使用实际部署的历史名称，不作重命名。

唯一目标：`cc.thkl.site`，域名 `llmapi.thkl.site`。基线为 `v0.2.9-traffic-trend-dashboard-326a74ac3`，Compose 目录 `/home/thinkerlu/sub2api-deploy`，使用 `docker-compose.yml` 与 `docker-compose.host-access.yml` 两个文件。禁止在主服务器运行。

源码从干净的 `codex/release-sub2api` 提交归档后构建，manifest 分别记录源码提交、镜像 ID/config digest、归档摘要、基线与回滚镜像。包内 `features/` 是随源码合入的功能文档，`checks/` 保存本次构建的验证结果。

## 上传后与部署

完整目录应位于 `/home/thinkerlu/sub2api-deploy/releases/dual-leg-thinking-ws-v028-beta1-<commit9>/`。先在该目录执行：

```sh
sha256sum -c SHA256SUMS
test -x deploy.sh && test -x rollback.sh
sudo python3 release.py preflight
sudo ./deploy.sh
```

预检核对目标、运行/配置版本、基线镜像摘要、Redis、迁移记录和备份空间。首次部署要求新增路由开关关闭；思考回传兼容修复和使用记录新字段随版本生效，双链路及自动切号默认关闭。部署前保存 Compose、`.env`、旧镜像和数据库 pg_dump，备份仅保留在目标机私有 `backups/`，不得上传或纳入 Git。

应用启动仅新增 `242_usage_log_ingress_transport.sql`：历史值为 0，不回填、不改计费键。健康检查覆盖本机端口和 `https://llmapi.thkl.site/health`；真实上游连通和灰度效果须在服务部署后另行验证，本次上传不等于已经部署。

## 灰度配置

配置名见 `features/2026-09-26-dual-leg-responses-ws.md` 与其他功能文档。启用双链路必须显式设置进程/账号内存预算，保留容器限额中的进程基线和安全余量；账号 mode 和已验证协议档案决定上游候选。自动切号另需分组与客户端版本 allowlist。当前隔离客户端验证为 Codex CLI `0.155.0-alpha.16.4`。

本地 Linux/amd64 容器压力测试只覆盖 16 MiB 合成请求和并发预算拒绝；manifest 记录实际峰值 RSS、容器限额与宿主架构。不得据此承诺生产并发容量或跨实例总内存上限。所有新路由开关在初次发布中保持关闭，按实际流量逐步验证。

## 回滚

```sh
sudo ./rollback.sh
```

回滚校验当前镜像和备份，即使应用容器已经停止也可执行。先停止应用接收新 WS，并等待 Redis 中迁移/去重窗口排空，最多 130 秒，然后恢复旧配置和旧镜像。状态不能排空时保持应用停止并报错，不清空恢复状态来强行接管。

数据库业务数据不回退。只允许 manifest 声明的 additive 迁移差异，保留 `ingress_transport` 列与已记录值；旧程序列列表已通过真实 PostgreSQL 兼容验证。既有迁移变更或出现其他新增迁移时拒绝自动回滚。回滚会恢复备份 `.env`，新账号 extra 字段仍保留，旧版会忽略这些字段。

## 重建

在发布 worktree 运行 `python3 tools/releases/dual-leg-thinking-ws-v028-beta1/build_package.py`。构建要求本地 Go 1.27、已安装前端依赖、Docker context `colima-sub2api-build`、基线镜像和合成 Codex CLI。脚本运行后端回归、前端测试/类型检查、Redis CAS、真实 PostgreSQL、真实 CLI 恢复/不重试探针、竞态检查、Linux 内存压力及镜像内嵌前端冒烟。
