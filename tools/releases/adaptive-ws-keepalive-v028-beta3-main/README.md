# Beta3 主服务器专用发布包

唯一目标：`ac.thinkerlu.top`（主机名 `vm75142.akkocloud.com`），域名 `subapi.thinkerlu.top`，Compose 目录 `/home/thinkerlu/sub2api-deploy`。仅使用 `docker-compose.yml`，不套用备用机覆盖文件或 18080 健康端口。

应用镜像沿用已验证的 `v0.2.8-beta3-adaptive-ws-keepalive`，构建提交 `b3a100e05054b265a2e0a8a71827c659553503e9`，镜像归档未重建。manifest 的 `deployment_scripts_commit` 单独记录本机专用脚本来源，不冒充应用构建提交。

已只读核对主机当前运行 `v0.2.8-request-traffic-monitoring`，提交 `7a3472327b70c196089fc07c44ec5d08a3debb62`，二进制摘要匹配主机既有发布 manifest。Docker 镜像摘要、运行 Compose 标签、数据库和备份空间的最终预检需要 sudo，由执行者运行。

## 部署

```sh
cd /home/thinkerlu/sub2api-releases/adaptive-ws-keepalive-v028-beta3-main-b3a100e05
sha256sum -c SHA256SUMS && sudo python3 release.py preflight
sudo ./deploy.sh
```

只有预检成功才执行 deploy.sh。脚本核对主机、应用/镜像/二进制基线、Redis、迁移和空间；先备份单 Compose、.env、旧镜像和数据库到目标机私有目录，再停止应用、排空恢复状态并切换镜像。部署会中断请求，窗口排空最多 130 秒。失败尝试恢复旧应用，业务数据库不倒退。

与备用机不同，本机升级新增 `242_usage_log_ingress_transport.sql`。只允许这一项 additive 迁移，旧迁移摘要不得改变。应用自动迁移，不手工执行 SQL。其余功能包含 beta3 保活修复、beta2 移除新增环境开关和内存预算，以及此前流量趋势等累计改动；详细功能文档在 features/。

健康验证使用容器内 8080 和主服务器域名；不修改现有网络、端口、容器资源和模型注册表挂载。部署后需重新连接客户端并验证真实请求，尤其是两轮之间空闲五分钟后的 GPT WS 保活。健康接口通过不代替真实上游验收。

## 回滚

### 空间不足时显式跳过数据库备份

默认仍备份数据库。经操作人接受风险后，可以改用：

```sh
sha256sum -c SHA256SUMS && sudo python3 release.py preflight --skip-database-backup && sudo ./deploy.sh --skip-database-backup
```

预检和部署两次都必须带该参数。只取消数据库 dump，保留配置、旧镜像备份和迁移检查。backup.json 明确记录数据库备份已跳过，摘要为 null。本次发布无法提供数据库灾难恢复，应用回滚仍保留业务数据和新增列，不需要数据库 dump。回滚命令不变。

按操作人要求，磁盘空间只输出当前可用字节数，不再按估算值拒绝部署；无论是否跳过数据库备份都不设置硬性门槛。实际写入、备份或镜像导入失败仍会中止，磁盘耗尽也可能影响当前服务和回滚。脚本不会自动删除已有备份或镜像。

### 执行回滚

```sh
sudo ./rollback.sh
```

恢复主机原 `v0.2.8-request-traffic-monitoring-7a3472327` 镜像和备份配置，不回滚数据库数据；保留新 ingress_transport 列及已写入记录。旧版列列表兼容性已通过原 beta3 发布包的 PostgreSQL 探针验证。出现其他迁移或恢复状态未排空时停止自动回滚。该脚本不可用于备用机。

## 组包来源

在干净发布 worktree 执行 `python3 tools/releases/adaptive-ws-keepalive-v028-beta3-main/assemble_package.py`。先校验原 beta3 完整包，再复用镜像归档，生成主机 manifest、源码差异、脚本差异、目标专用说明和校验文件。原应用测试日志保留，主机脚本测试另存 `checks/main-release-tests.log`。
