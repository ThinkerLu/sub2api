# v0.2.4 内存优化与 WS TTFT 发布包

此目录随发布包提供 `manifest.json`、Docker 镜像归档、SHA256SUMS。服务器路径由交付说明指定。

部署只切换 sub2api 镜像，不删除数据库、Redis 或 data，不修改现有环境参数。不自动清理旧镜像。当前回滚基线固定为 `63c6fa1da699400cf8db842653f90e3c9d7706b4`（memory-v2）；执行前校验实际运行版本，不能用于其他基线。

```sh
sha256sum -c SHA256SUMS
sudo ./deploy.sh --deploy-dir /home/thinkerlu/sub2api-deploy
# 需要回滚时：
sudo ./rollback.sh --deploy-dir /home/thinkerlu/sub2api-deploy
```

部署需要 Docker 管理权限，至少 350 MiB 可用空间。校验归档及镜像架构/程序提交后，停止应用保存实际容器快照和配置；不停止数据库。保留迁移记录，健康检查或版本检查失败时恢复旧快照。数据库结构必须未变化；检测到漂移时拒绝自动回滚，不自动恢复数据库备份。全程使用部署锁，重复执行不会覆盖第一次保存的回滚基线。

`python3 test_release.py` 可运行本地模拟演练：部署/回滚/重部署、快照失败、空间不足、健康失败、归档校验失败、错误基线、迁移漂移、配置保留以及镜像摘要兼容。演练不会连接生产服务。
