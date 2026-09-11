# Adaptive WS 预热连接复用发布

修复 generate=false 小预热让连接提前进入持续 passthrough，导致随后大正式请求被上游 WS 拒绝的问题。

预热使用真实上游 WS，保留响应 ID、用量和连接；连续预热复用同一上游连接。正式请求到来后按恢复上下文及实际待发送 WS 帧大小选协议：大正式请求关闭预热连接后 HTTP 桥接，小正式请求复用原上游 WS，保留引用和增量，避免重复历史。客户端 WS 始终保持打开。桥接期间继续维护增量上下文，压缩后恢复持续 passthrough。没有预热的小首帧仍保持原直接 passthrough 行为。

预热不支持 HTTP 的 generate=false 语义；如果预热本身恢复后的完整请求已达到 WS 阈值，会明确失败，不能删除 generate=false 后误触发真实生成，也不把超限消息发往上游 WS。本次修复针对实际观察到的小预热、大正式请求序列。只识别显式布尔 generate=false，不凭零输出 token 猜测预热。

## 部署与回滚

进入本发布目录后执行：

```sh
sha256sum -c SHA256SUMS
sudo python3 release.py preflight
sudo ./deploy.sh
# 需要回滚时
sudo ./rollback.sh
```

默认 Compose 目录 /home/thinkerlu/sub2api-deploy，可用 --deploy-dir 指定。仅接受实际运行基线 312458262f957e53c0dd7e16616f50083849ed9b（v0.2.4-ws-adaptive）；部署前保存当前实际容器镜像和 Compose/.env。仅切换 sub2api 服务，120 秒健康检查失败会尝试自动恢复。

回滚恢复本次部署前的 adaptive 镜像与配置，**不把账号 adaptive 改成 passthrough**，不删除数据库列或数据。本次没有新增迁移。需要与之前版本不同的 .ws-adaptive-warmup-reuse-v024-backup-path，旧发布回滚记录保留。

至少需要512 MiB磁盘余量（上传后计算），以及管理员sudo权限。脚本不清理任何镜像或数据卷，不执行数据库全量备份。当前能力适配现有单实例Compose内置PostgreSQL/Redis拓扑。

## 验证

```sh
python3 -m unittest discover -s . -p test_release.py -v
```

包内附源码bundle/patch、前后端构建日志、回归/race记录、Linux镜像冒烟记录。功能测试覆盖预热引用恢复、65 MiB正式请求、压缩切回WS、逐轮协议和回调、失败、取消与重叠请求；relay验证单轮结束不会消耗下一条客户端消息或关闭下游。

本次同时包含逐轮 generate/预热元数据日志和连接 reused 标记。真实 WebSocket 回归断言两次预热、正式请求及后续增量只建立一次上游连接；退出、失败和空闲超时释放连接。已进入正式业务passthrough之后，仍不支持缺少历史时任意反向桥接。

本目录为独立发布，使用专属回滚指针，不覆盖旧预热包或旧包回滚记录。旧包尚未部署，回滚基线仍为 312458262。
