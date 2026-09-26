# v0.2.8-beta2-dual-leg-default 备用机发布包

唯一目标：`cc.thkl.site`，域名 `llmapi.thkl.site`。基线为 `sub2api:v0.2.8-beta1-dual-leg-thinking-ws-3fdfb9bb4`，Compose 目录 `/home/thinkerlu/sub2api-deploy`。禁止在主服务器运行。

移除 beta1 新增的功能开关、恢复灰度名单及内存预算，双链路默认生效。原有基础 WS 开关、账号协议能力、单帧限制、并发/连接数限制继续有效。不会把 Chat Completions 自动变成 Responses。详见 `features/2026-09-26-dual-leg-default-no-budget.md`。

## 部署

进入 `/home/thinkerlu/sub2api-deploy/releases/dual-leg-default-v028-beta2-<commit9>/`：

```sh
sha256sum -c SHA256SUMS && sudo python3 release.py preflight
sudo ./deploy.sh
```

预检不通过时停止。脚本验证包、版本/镜像摘要、迁移、空间、Redis 和健康；备份 Compose/.env/数据库/旧镜像到目标机私有目录，停止应用并等待恢复窗口排空（最多 130 秒），再切换镜像。失败尝试恢复 beta1。操作会中断请求，建议低流量时执行。不得输出私有备份或生产秘密。

无数据库迁移，包内 242 SQL 仅作既有结构参考，不手工执行。升级后不再运行 beta1 配置脚本，无需新增变量。旧自定义变量被程序忽略；保留原配置便于回滚，不改变既有容器内存限制。

部署后检查本机 `http://127.0.0.1:18080/health`、域名 `https://llmapi.thkl.site/health`、程序版本，再用短请求验证 GPT 与原生 Responses 账号，观察 `dual_leg_route` 和使用记录的入站/上游传输。健康检查不代替真实上游验收。

## 回滚

```sh
sudo ./rollback.sh
```

恢复 beta1 镜像和备份配置，保留业务数据。窗口排空失败或迁移列表改变时拒绝自动回滚。beta1 双链路是否启用取决于备份配置。

## 重建

从干净 `codex/release-sub2api` 运行 `python3 tools/releases/dual-leg-default-v028-beta2/build_package.py`。最终组包调用真实 `verify_package`，防止版本校验漏改。内存探针在 1 GiB Linux/amd64 容器中测试 16 MiB 请求，不启用预算，不构成生产容量承诺。
