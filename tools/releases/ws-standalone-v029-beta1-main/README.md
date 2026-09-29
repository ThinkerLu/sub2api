# v0.2.9-beta1 主服务器发布包（交付工具 r1）

完整版本为 `v0.2.9-beta1-ws-standalone-named-output`。唯一目标：`ac.thinkerlu.top`（主机名 `vm75142.akkocloud.com`），域名 `subapi.thinkerlu.top`，Compose 目录 `/home/thinkerlu/sub2api-deploy`。仅使用单文件 `docker-compose.yml`，不得套用备用机覆盖文件或 18080 健康端口。

官方源码基线为 `v0.2.9`（`4c00df2e0183e2c70b7fa8ba45914205e36aad0c`，tree `212ecc21a885afd84eb9ff70df2bf5d543b864eb`）。应用源构建提交、镜像标签与摘要、归档 SHA256、发布脚本提交及回滚基线以包内 `manifest.json` 为准。

主服务器部署前运行 `v0.2.8-beta3-adaptive-ws-keepalive-b3a100e05`。发布包以该镜像作为回滚基线，不会把主服务器回滚到备用机的流量监控镜像。

## 上传与预检

完整发布目录应位于 `/home/thinkerlu/sub2api-releases/ws-standalone-v029-beta1-main-<commit9>/`。上传不等于部署。先执行：

```sh
sha256sum -c SHA256SUMS
sudo python3 release.py preflight
```

预检核对主机名、单 Compose 配置、运行镜像与源码提交、二进制摘要、Redis、迁移记录、域名健康和发布包摘要。磁盘空间只输出当前可用字节数，不设置估算准入门槛；写入、备份或镜像导入失败仍会中止。预检不做任何服务切换。

## 部署

只有预检通过后才执行：

```sh
sudo ./deploy.sh
```

默认在切换前备份 `.env`、`docker-compose.yml`、旧镜像、数据库 `pg_dump -Fc` 和迁移摘要。旧镜像通过 `docker save | gzip -1` 直接写成压缩归档，不生成同等大小的未压缩 tar；部署前还会清理未被发布指针引用的未完成备份目录。经操作人明确接受风险时，可同时给预检和部署传入 `--skip-database-backup`；配置和旧镜像仍会备份，但发布包不再提供数据库灾难恢复。部署会停止应用并排空 WS 恢复窗口，最多等待 130 秒，期间请求会中断。

本版本不新增或改写数据库迁移，部署后要求迁移记录与部署前完全一致。健康验证使用容器内 8080 和主服务器域名。部署后还需重新连接客户端并验证真实请求；健康接口通过不等于上游业务验收。

## 回滚

```sh
sudo ./rollback.sh
```

脚本从压缩归档恢复 beta3 应用镜像，并恢复 `.env` 和 Compose 文件；数据库与发布后业务数据保留，不执行数据库倒退。若迁移记录发生变化，脚本拒绝自动回滚并保留当前状态供人工处理。

## 组包来源

在干净的 `codex/release-sub2api` worktree 执行：

```sh
python3 tools/releases/ws-standalone-v029-beta1-main/build_package.py
```

脚本编译并测试发布分支，构建完整的 Linux/amd64 应用镜像，生成发布包与 SHA256SUMS，再调用包内真实的 `release.py` 校验 manifest、脚本、归档和文件摘要。
