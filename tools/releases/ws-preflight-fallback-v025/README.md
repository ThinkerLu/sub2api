# v0.2.5 WS 预检与 HTTP 回退修复发布

本发布基于线上已运行的 v0.2.5 model-registry 版本，增加两项修复：

- WS 握手预检复用调度器的真实资格判断，排除额度暂停、运行时阻塞、模型不支持、分组不兼容和非原生 WS 账号。
- OpenAI OAuth 自动透传账号不再声称支持 DeepSeek、GLM、Grok 等其他厂商模型；显式 `model_mapping` 仍可作为支持声明。

因此，当某个模型只有第三方 API Key 上游可用、而该账号的 WS mode 为 `off` 时，网关会在握手阶段返回 HTTP 426，Codex 立即切换到 HTTP，不再先执行 5 次 WS 重试。

外置 model registry 的目录、文件、价格和能力配置保持不变。部署只切换应用镜像，不覆盖运营已维护的 `model-registry/model-registry.yaml`。

## 部署与回滚

进入本发布目录后执行：

```sh
sha256sum -c SHA256SUMS
sudo python3 release.py preflight
sudo ./deploy.sh

# 外部文件修改后立即重载
sudo ./reload.sh

# 回滚
sudo ./rollback.sh
```

默认 Compose 目录为 `/home/thinkerlu/sub2api-deploy`，可用 `--deploy-dir` 指定。部署脚本只接受当前基线 `4700dbdf6`，部署前保存实际容器、Compose 和 `.env`；回滚恢复该基线镜像，保留数据库、业务数据和外置注册表目录。

## 验证

```sh
python3 -m unittest discover -s . -p test_release.py -v
```

发布脚本测试覆盖外部注册表保留、Compose 挂载、SIGHUP 重载和回滚保留挂载。后端测试覆盖额度暂停账号不参与 WS 预检，以及透传 OAuth 账号不误声明其他厂商模型。
