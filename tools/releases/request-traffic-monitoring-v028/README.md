# v0.2.8 请求流量与会话筛选发布

本发布基于当前线上的 `v0.2.5-ws-turn-metadata-fallback-737912b14`，合并上游 `v0.2.8`，并加入请求级应用层正文字节统计、会话 ID 列与筛选、以及仪表盘流量统计。

## 功能变更

- 新增 `ingress_body_bytes`、`upstream_request_body_bytes`、`upstream_response_body_bytes`、`egress_body_bytes`。
- 用量明细新增“流量”列，展示四个方向并按 KB/MB/GB 友好显示。
- 用量明细新增“会话ID”列、复制按钮和完整会话 ID 筛选。
- 管理员仪表盘新增“流量”块，展示四个方向的累计值与今日值。
- Excel/CSV 导出包含流量与会话 ID 字段。
- 流量为应用层正文大小，不包含 TCP/TLS、HTTP Header、WS Frame 和重传开销。

## 部署与回滚

进入本发布目录后执行：

```sh
sha256sum -c SHA256SUMS
sudo python3 release.py preflight
sudo ./deploy.sh

# 回滚
sudo ./rollback.sh
```

默认 Compose 目录为 `/home/thinkerlu/sub2api-deploy`，可用 `--deploy-dir` 指定。部署脚本只接受当前基线 `737912b14`，部署前保存实际容器、Compose、`.env` 和迁移快照；回滚恢复该基线镜像，保留数据库、业务数据和外置 model registry。

本版本新增四个幂等增量迁移：

- `238b_content_moderation_engine_meta.sql`
- `239_channel_reasoning_effort_multipliers.sql`
- `240_affiliate_ledger_operation_id.sql`
- `241_usage_log_traffic_bytes.sql`

## 验证

```sh
python3 -m unittest discover -s . -p test_release.py -v
```

Linux amd64 镜像内会运行流量计数器、HTTP 上游流量、会话筛选和 WS 中间件的定向测试，并检查内嵌前端启动。
