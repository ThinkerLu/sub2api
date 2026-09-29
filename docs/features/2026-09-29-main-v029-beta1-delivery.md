# 主服务器 v0.2.9 beta1 发布与回滚

目标版本：`v0.2.9-beta1-ws-standalone-named-output`。官方源码基线为 `v0.2.9`，tag 指向提交 `4c00df2e0183e2c70b7fa8ba45914205e36aad0c`，完整 tree 为 `212ecc21a885afd84eb9ff70df2bf5d543b864eb`。应用源构建提交、镜像标签与摘要、归档 SHA256 及脚本来源提交由发布包 `manifest.json` 记录。

## 目的

把发布分支已有的 WS 具名独立输入修复与累计自定义改动，建立在官方 `v0.2.9` 代码之上，面向主服务器 `ac.thinkerlu.top` 生成完整镜像、部署脚本和回滚脚本。本次只上传发布包，不执行部署。

## 关键行为

- 发布分支通过本地保存的 `v0.2.9` 完整 tree 与 `v0.2.8` 基线做三方合并；上游 tag 元数据和 tree 摘要写入 manifest。
- 新镜像从发布分支提交构建，运行版本注入为 `v0.2.9-beta1-ws-standalone-named-output`。
- 回滚基线是主服务器当前运行的 `sub2api:v0.2.8-beta3-adaptive-ws-keepalive-b3a100e05`，不是备用机的 v0.2.8 流量监控镜像。
- 本发布不新增或改写数据库迁移。部署和回滚都要求 `schema_migrations` 内容完全不变。
- 部署脚本先备份 `.env`、单 Compose 文件、旧镜像和数据库，再排空 WS 恢复窗口并切换应用镜像；回滚保留业务数据库和现有迁移记录。

## 配置和数据

无新增配置项、无 API 变更、无数据库迁移。部署不修改主服务器现有网络、端口、容器资源和模型注册表挂载。

## 验证方法

在发布 worktree 执行：

```sh
python3 tools/releases/ws-standalone-v029-beta1-main/build_package.py
```

构建脚本运行后端 WS 相关测试、后端全包编译、前端目标测试、类型检查、前端构建、镜像内嵌前端冒烟与发布脚本单测，并校验最终发布包摘要。目标机预检必须在包内目录执行：

```sh
sha256sum -c SHA256SUMS
sudo python3 release.py preflight
```

只有预检通过才运行 `sudo ./deploy.sh`。部署后需检查 `https://subapi.thinkerlu.top/health`、运行版本、迁移摘要和真实 WS 请求。上传、预检或健康检查通过都不代替真实上游验收。

## 发布与回滚

完整包上传到 `/home/thinkerlu/sub2api-releases/ws-standalone-v029-beta1-main-<commit9>/`。部署会中断请求，应安排在低流量窗口。

回滚执行：

```sh
sudo ./rollback.sh
```

脚本恢复 beta3 应用镜像、`.env` 和 Compose 文件，不恢复数据库快照，不删除发布后的业务数据。若迁移记录与部署前备份不一致，脚本拒绝自动回滚。

## 来源

发布工具与说明位于 `tools/releases/ws-standalone-v029-beta1-main/`。功能文档索引位于 `docs/features/README.md`，详细行为见 `2026-09-29-ws-standalone-named-output.md` 及其上游累积功能文档。
