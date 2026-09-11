# Adaptive WS 发布包

应用提交 `312458262f957e53c0dd7e16616f50083849ed9b`，镜像 `sub2api:v0.2.4-ws-adaptive-312458262`，Linux amd64。后端与前端均由该提交构建。回滚基线为 `257eee18e7aa177f08eeba77c8d86e4bdd2d4661`（v0.2.4-memory-ttft）。

## 服务器操作

```sh
cd /home/thinkerlu/sub2api-releases/ws-adaptive-v024-312458262
sha256sum -c SHA256SUMS
sudo python3 release.py preflight
sudo ./deploy.sh
# 需要回滚时
sudo ./rollback.sh
```

默认 Compose 目录 `/home/thinkerlu/sub2api-deploy`，可通过 `--deploy-dir` 指定。预检不导入镜像或切换应用；部署需要管理员 sudo。部署前至少保留 **512 MiB** 可用空间（发布包上传之后计算），检查不通过就退出。不要删除正在运行或回滚所需的镜像；本包不自动清理磁盘。

部署会检查运行提交、归档摘要、镜像架构/ID/二进制提交、Compose、数据库、Redis 和空间，保存配置及当前实际容器镜像快照，然后仅重建 sub2api 服务。保持现有配置，不自动启用 adaptive。120 秒健康检查失败会尝试恢复基线。切换会中断已有连接；脚本按 120 秒停止超时等待应用退出。

此脚本面向单实例、当前 Compose 内置 PostgreSQL/Redis 拓扑。不要并行运行其他发布/账号配置修改。故障恢复中断时会保留快照与错误信息，修复报错后重试 rollback.sh。脚本不删除业务数据或镜像，不执行数据库全量备份；配置与容器快照不等同数据库备份。迁移仅新增一列，回滚不恢复历史数据库快照，避免覆盖部署后的新记录。

## 数据库及账号回滚

迁移 238 新增 `usage_logs.upstream_transport SMALLINT NOT NULL DEFAULT 0`，0=未知、1=HTTP、2=WebSocket；不新增 request_type=6。回滚保留该列、迁移记录及所有用量数据。脚本只允许与基线相比增加已校验 checksum 的迁移 238，其他迁移变化会阻止自动回滚。

回滚先停止新应用，将 OpenAI 账号两个 WS mode 字段中仍为 adaptive 的值改成 passthrough；其他配置不变。受影响账号 ID 和原模式备份在回滚目录 `adaptive-modes.before-rollback.json`。同时写入 scheduler outbox，并定向失效这些账号的 Redis 调度快照，随后恢复旧镜像与原 Compose/.env。重复回滚保留第一次模式备份并重试缓存失效。

再次部署不会自动恢复 adaptive，需要在账号配置中重新选择。启用本功能仍需既有 WS 开关与 mode router v2；不依赖旧 http_bridge_enabled。只在 adaptive 路径取消本地 64 MiB 消息上限；15 MiB 是默认协议选择阈值，上游自身限制仍然生效。

## 验证与重现

```sh
python3 -m unittest discover -s . -p test_release.py -v
# 仅在本地测试机，要求 Docker 已有 postgres:18-alpine 和 redis:8-alpine：
python3 validate_postgres.py
```

模拟测试覆盖部署/回滚/重部署、健康失败恢复、空间/校验失败、迁移漂移与模式备份/缓存失效。PostgreSQL/Redis 演练创建隔离临时容器，不映射端口、不读取生产配置，结束后删除测试容器。它验证新增列及旧列读写兼容、针对性模式转换与幂等重试；不是生产数据库演练。

发布包还附带构建脚本、源代码 patch/bundle、前端构建日志、镜像验证、Linux TTFT 回归，以及本次功能的后端/race/前端与内存诊断记录。`build_package.py` 使用已校验旧运行时镜像，重新编译前后端；所需本地 Go/Node/Colima 路径在脚本中注明，先用 pnpm 10 按 frozen lockfile 安装前端依赖，再设 SUB2API_RELEASE_COMMIT 执行。
