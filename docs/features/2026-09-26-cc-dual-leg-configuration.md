# 备用机双链路配置与诊断留档

> 此配置工具仅用于 beta1。beta2 已移除这些新增开关和预算，不再执行本文启用命令；见 [beta2 变更](2026-09-26-dual-leg-default-no-budget.md)。保留本文用于故障与运维历史留档。

适用版本：`v0.2.8-beta1-dual-leg-thinking-ws`，应用提交 `3fdfb9bb427eb6d23a17aa471c6c3c15e2821a2b`。工具修订 r1，开发分支 `codex/cc-dual-leg-config`（尚未提交）。仅提供既有功能的运维配置，不发布新应用镜像或改变版本。

## 诊断记录

2026-09-26 北京时间 17:00，任务 `01a0dcf1-4b62-74b1-a7a0-8f2f728afb41` 使用 `deepseek-flash`。客户端 WS 握手成功，随后连接被关闭；5 次重试后于 17:00:23 回退 HTTP，对应 HTTP 请求最终返回 200。

备用机基础 WS 和 mode router v2 已开启，但双链路及两项预算均未配置。仅发送 `generate:false` 预热帧复现：101 握手成功，随后关闭码 1008，原因 `Responses WebSocket API only supports OpenAI-compatible models for composite groups`。该拒绝来自双链路关闭时的合成分组平台准入，不是 Caddy Upgrade 失败。

诊断未修改配置或重启服务。原始日志在本机 Codex 日志库及备用机 `data/logs/sub2api.log`；本文不复制请求正文、密钥或用户数据。

## 范围与前提

- 唯一目标 `cc.thkl.site`，已核对主机名 `cc.thinkerlu.online`；只接受上述 beta1 镜像及摘要。
- 脚本源码：`tools/operations/cc-dual-leg/configure.py`。依赖 Python 3、PyYAML、Docker Compose、curl 和 sudo。备用机已确认有 PyYAML。
- 修改现有 `docker-compose.host-access.yml` 的 sub2api 环境与内存限制，保留 extra_hosts、其他服务和变量插值。不是仅写 `.env`，不引入第三个 Compose 文件。
- apply/rollback 都会重建应用容器并中断当前请求/WS，请在低流量窗口操作。不更换镜像、不操作数据库、不重启 Redis/PostgreSQL。
- 本次仅开启逐轮双链路，自动切号/切模型恢复明确保持关闭。不要同时手工开启恢复开关。

## 先核对账号

在管理后台编辑提供 `deepseek-flash` 的 DeepSeek 账号：

1. 上游必须真正支持原生 HTTP Responses。协议选 Responses，或已验证可选择 Responses 的 Adaptive。仅有 Chat Completions 的端点不因打开双链路而自动兼容。
2. 仅支持 HTTP Responses 时，账号 `responses_ws_mode` 选 `http_bridge`，不填猜测的 WS 档案/URL。预期链路为客户端 WS、上游 HTTP。
3. 只有上游明确支持 Responses WS v2，才选 `adaptive`，并设置 `responses_ws_profile=openai_responses_v2`、完整 `responses_ws_url` 及对应 HTTP Responses URL。
4. 检查合成分组模型归属、账号分组、白名单和映射。脚本不修改账号数据、不代替上游能力验证。

## 操作步骤

独立工具包目录：`/home/thinkerlu/sub2api-deploy/releases/cc-dual-leg-config-r1-20260926/`。无需重跑应用包的 deploy.sh。

```sh
cd /home/thinkerlu/sub2api-deploy/releases/cc-dual-leg-config-r1-20260926
sha256sum -c SHA256SUMS
sudo python3 configure.py status
sudo python3 configure.py preflight --process-mib 512 --account-mib 512 --container-mib 1024
```

512/512/1024 MiB 仅是单账号低并发起点，不是容量承诺。账号共享进程预算；大帧有约 8 倍工作集及额外预留，预算不足会拒绝请求。降低账号预算可隔离账号，但大帧也更容易被拒绝。

预检要求容器覆盖当前 RSS + 进程预算 + 256 MiB 余量、宿主内存保留 20%，并检查镜像、版本、候选 Compose 的实际环境注入、本机与域名健康。临时校验文件用后清理，不修改运行配置、不重启；不通过时不得盲目提高额度。

预检通过、账号能力已确认后，使用同一组参数：

```sh
sudo python3 configure.py apply --process-mib 512 --account-mib 512 --container-mib 1024
sudo python3 configure.py status
```

脚本先备份 Compose 两文件和 `.env` 到私有 `backups/cc-dual-leg-config-*/`（目录 0700、文件 0600），原子替换 overlay、重建应用，再校验运行环境、内存限制和健康。失败自动尝试恢复；恢复失败保留活动备份指针。私有备份不得上传、提交或贴入聊天。

## 验收与配置回滚

重新建立客户端连接，先短请求测试 `deepseek-flash`，再测多轮与 GPT/DeepSeek 切换。核对使用记录的入站 WS/上游传输、服务端 `dual_leg_route`、响应终止事件及客户端有无 `falling back to HTTP`。健康检查不代表真实上游成功；不要用敏感长历史做首轮验收。

```sh
sudo python3 configure.py rollback
sudo python3 configure.py status
```

该命令只恢复本次启用前的 overlay，不回退镜像、账号数据或数据库。主 Compose、`.env` 或 overlay 被后续修改时拒绝覆盖，需人工比较备份。恢复完成保留备份目录，仅清除活动指针。整个应用版本回滚仍使用 beta1 发布包的 rollback.sh。

## 验证与交付记录

测试覆盖原配置保留、列表环境、预算拒绝、双内存限制一致性、原子写权限、错误镜像拒绝、失败恢复和恢复失败保留指针。本地可在脚本目录执行 `uv run --with pyyaml python -m unittest -v test_configure.py`。

本次仅准备及上传工具，不启用功能。完整 sudo 预检和真实业务验收需操作者执行。工具包包含源码、本文、manifest、测试记录和 SHA256SUMS；manifest 分开记录应用基线与工具来源，未提交源码不冒充发布提交。

本次实测结果：本地及备用机各 8 项单测全部通过；备用机真实 Docker Compose 对现有 overlay 临时副本的合并测试通过，确认环境注入和 1 GiB 内存限制，实际 overlay 摘要前后不变。该机 Compose JSON 的 `mem_limit` 为字符串，脚本按整数转换后比较。未执行 sudo preflight/apply，也未修改账号协议或容器状态。
