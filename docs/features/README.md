# 功能索引

HTTP→WS 流量统计：[上游流量统计修复](2026-10-01-http-ws-traffic-accounting.md)，首次 `v0.2.11-probe19-http-ws-traffic`，仅备用机发布分支。

原生 WS 分组兼容：[命名空间替换与原生 WS](2026-10-01-codex-v2-native-ws.md)，
目标版本 `v0.2.8-probe17-codex-v2-native-ws`（仅备用机探针）。

主服务器交付：[beta3 专用部署和回滚基线](2026-09-26-main-beta3-delivery.md)。

beta3 修复：[Adaptive 上游 WS 轮间保活](2026-09-26-adaptive-ws-upstream-keepalive.md)。

当前 beta2 行为：[双链路默认生效、移除新增开关与内存预算](2026-09-26-dual-leg-default-no-budget.md)。beta1 文档保留历史记录，冲突处以此文档为准。

| 功能 | 文档 | 首次发布 |
| --- | --- | --- |
| v0.2.11 整合与 WS 上游错误透传 | [上游版本与错误透传](2026-10-01-v0211-upstream-errors.md) | v0.2.11-beta1-upstream-errors（已合入发布分支，未发布） |
| 仪表盘流量趋势 | [流量趋势](2026-09-25-traffic-trend-dashboard.md) | v0.2.9-traffic-trend-dashboard |
| 目标模型思考回传 | [思考回传](2026-09-26-target-thinking-replay.md) | v0.2.8-beta1-dual-leg-thinking-ws |
| GPT 阈值选路 | [大小判据](2026-09-26-gpt-ws-threshold-routing.md) | v0.2.8-beta1-dual-leg-thinking-ws |
| 逐轮双链路与内存准入 | [双链路](2026-09-26-dual-leg-responses-ws.md) | v0.2.8-beta1-dual-leg-thinking-ws |
| 严格归属与共享恢复状态 | [恢复状态](2026-09-26-ws-recovery-state.md) | v0.2.8-beta1-dual-leg-thinking-ws |
| 切模型全量恢复 | [切模型重连](2026-09-26-ws-model-switch-full-retry.md) | v0.2.8-beta1-dual-leg-thinking-ws |
| 显式协议档案与生命周期 | [账号 WS 档案](2026-09-26-responses-ws-profiles.md) | v0.2.8-beta1-dual-leg-thinking-ws |
| 使用记录双向传输 | [入站传输](2026-09-26-usage-ingress-transport.md) | v0.2.8-beta1-dual-leg-thinking-ws |
| WS 覆盖率判定识别具名独立输入 | [具名独立输入](2026-09-29-ws-standalone-named-output.md) | v0.2.9-beta1-ws-standalone-named-output |
| 非原生 WS 上行跳过错路握手预检 | [握手预检](2026-09-29-ws-preflight-transport.md) | v0.2.9-beta1-ws-standalone-named-output |
| v0.2.9 发布工具压缩旧镜像备份 | [压缩备份](2026-09-29-v029-image-backup-compression.md) | v0.2.9-beta1-ws-standalone-named-output（工具 r2） |
| 主服务器 v0.2.9 beta1 发布与回滚 | [主服务器交付](2026-09-29-main-v029-beta1-delivery.md) | v0.2.9-beta1-ws-standalone-named-output |
| Adaptive WS 上下文累积策略 | [上下文策略](2026-09-30-adaptive-ws-context-accumulation-policy.md) | 待定（默认关闭，可热重载） |
| 流量趋势指标对配色与差值显示 | [指标对配色](2026-09-30-traffic-trend-pair-colors.md) | 待定（默认生效，已合入发布分支） |

v0.2.8-beta1 的打包与目标机操作见源码仓库 `tools/releases/dual-leg-thinking-ws-v028-beta1/README.md`。上传不代表已经部署；实际构建提交、验证结果及镜像摘要以发布包 manifest 为准。
