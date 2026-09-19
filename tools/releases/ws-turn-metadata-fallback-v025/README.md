# v0.2.5 WebSocket turn-metadata HTTP 即时回退发布

本发布基于当前线上的 `v0.2.5-ws-preflight-fallback-17866c78b`，修复 composite 分组仍出现 5 次 WS 重试的问题。

Codex 的 WebSocket 升级请求通常不发送 `x-codex-routing-hint`，但会发送 `x-codex-turn-metadata`。此前服务端只能等 WebSocket 建成并读取首个 `response.create` 帧后才判断模型是否有原生 WS 路由，因此 Codex 会按普通流中断执行 `1/5` 到 `5/5` 重试。

本次改动在握手前同时读取：

- `x-codex-routing-hint`（兼容已有网关路径）
- `x-codex-turn-metadata.model`（Codex Desktop / CLI 实际发送路径）

若目标模型没有可用的原生 WS 账号，服务端会在升级前返回 HTTP 426，Codex 直接切换到 HTTP，不再先执行 5 次重试。请求首帧仍是最终权威模型，提前读取只用于传输方式预检。

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

默认 Compose 目录为 `/home/thinkerlu/sub2api-deploy`，可用 `--deploy-dir` 指定。部署脚本只接受当前基线 `17866c78b`，部署前保存实际容器、Compose 和 `.env`；回滚恢复该基线镜像，保留数据库、业务数据和外置注册表目录。

## 验证

```sh
python3 -m unittest discover -s . -p test_release.py -v
```

后端测试覆盖 turn metadata 模型解析、无 WS 账号时两种握手提示均返回 HTTP 426，以及此前的额度暂停和 OAuth 外厂商模型预检。真实 Codex 验收要求不得出现 `Reconnecting... 1/5` 到 `5/5`。
