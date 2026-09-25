# v0.2.9 流量趋势备用机发布

仅适用于 `cc.thkl.site` 当前运行的 v0.2.8 镜像 `7a3472327`。新镜像从发布分支提交构建；manifest 写明源码提交、镜像 ID 与归档 SHA256、目标基线和回滚镜像。

完整发布目录上传到 `/home/thinkerlu/sub2api-deploy/releases/traffic-trend-v029-<commit9>/` 后：

```sh
cd /home/thinkerlu/sub2api-deploy/releases/traffic-trend-v029-<commit9>
sha256sum -c SHA256SUMS
sudo python3 release.py preflight
sudo ./deploy.sh
# 如需回滚
sudo ./rollback.sh
```

脚本在修改服务前核对双 Compose 文件、实际运行镜像和提交、迁移记录、Redis、空间及归档摘要；部署前保存 `.env`、两个 Compose 文件、数据库 `pg_dump -Fc` 及旧镜像 tar，均存于目标机 `backups/`，不上传配置或数据库。新旧版本不包含新增迁移，回滚只恢复应用与配置，保留现有业务数据；若迁移记录变化则拒绝自动回滚。部署完成后还需验证域名 `https://llmapi.thkl.site/health`、仪表盘流量趋势和关键上游连接。请勿将备份或日志中的配置内容写入 Git。
