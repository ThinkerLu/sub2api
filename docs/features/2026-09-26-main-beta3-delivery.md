# Beta3 主服务器交付与回滚基线

目标版本：`v0.2.8-beta3-adaptive-ws-keepalive`。部署工具分支 `codex/beta3-main-delivery`，脚本提交见目标包 manifest 的 deployment_scripts_commit；应用构建提交保持 `b3a100e05`，不重新编号或重建已验证镜像。

本次用户明确选择主服务器 `ac.thinkerlu.top`，只上传完整包及专用部署/回滚脚本，不部署。已核对主机名 `vm75142.akkocloud.com`，实际运行 `v0.2.8-request-traffic-monitoring`、提交 `7a3472327b70c196089fc07c44ec5d08a3debb62`；运行二进制 SHA256 为 `89de10fe9b6d904d625e1307c439012579821176915ed8f2e56983dc041d97f8`，与主机历史发布 manifest 一致。

主机只有 docker-compose.yml，应用容器端口 8080；脚本保留网络及端口，在容器内检查健康并验证 `https://subapi.thinkerlu.top/health`，不使用备用机 host-access overlay 或主机 18080 端口。

从实际运行提交到 beta3 仅新增 242_usage_log_ingress_transport.sql。预检及回滚只允许该 additive 迁移；旧迁移摘要必须保持，新列和业务数据不回退。部署前备份配置、运行镜像、数据库；停止应用并排空 Redis 恢复窗口后切换；失败恢复旧版本。

脚本路径：`tools/releases/adaptive-ws-keepalive-v028-beta3-main/`。完整包上传到 `/home/thinkerlu/sub2api-releases/adaptive-ws-keepalive-v028-beta3-main-b3a100e05/`，操作步骤见包内 README.md。校验原 beta3 镜像及全部文件，目标脚本另测主机选择、单 Compose、容器及域名健康、版本/摘要拒绝和 additive 迁移回滚边界。

本机无免密 sudo，不能把只读基线检查说成完整部署预检通过。目标机执行者须先运行 `sudo python3 release.py preflight`，成功后才能部署。上传不改变运行镜像、配置或数据库；敏感备份只留目标机私有目录。

部署工具修订：`codex/main-optional-db-backup` 增加 `--skip-database-backup`，仅供操作人明确接受数据库无本次备份的风险时使用。预检与 deploy.sh 均须传入，默认仍备份；配置、旧镜像备份和迁移校验不变。backup.json 记录 skipped_by_operator 和空数据库摘要，回滚无需新增参数，仍不恢复或删除数据库。按操作人后续要求，取消所有估算空间准入门槛，仅提示当前可用空间及磁盘耗尽风险；实际写入、备份或导入失败仍会中止，不自动清理文件。测试覆盖默认备份、空备份拒绝、跳过不调用 pg_dump、不生成 dump、两种备份模式低空间均不阻断预检及部署参数传递。应用版本和镜像不变；只是部署工具修订，不构成新的应用发布，关联提交见 manifest 的 deployment_scripts_commit。
