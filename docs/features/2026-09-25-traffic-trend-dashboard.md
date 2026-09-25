# 仪表盘流量趋势

## 目的与使用

管理员仪表盘在“最近使用”下方整行展示流量趋势，替代原先的累计/今日流量卡片。沿用仪表盘的时间范围与小时/日粒度筛选，展示客户端上传、发往上游、上游返回、返回客户端及总量五条曲线。左轴仅由前四条方向曲线确定范围；右轴仅由总量曲线确定范围。图例可切换曲线，悬停查看各时段字节数。

## 数据与限制

- 扩展现有 `GET /api/v1/admin/dashboard/snapshot-v2` 的 `trend` 数据点与 `GET /api/v1/admin/dashboard/trend`：新增四个应用层正文字节数；总量在前端由四项相加，不增加数据库字段。
- 无额外配置或迁移，沿用既有 `usage_logs`、`usage_dashboard_hourly`、`usage_dashboard_daily` 的流量列。无筛选时优先使用预聚合数据；带筛选或聚合数据为空时读原始日志。时间范围与时区、粒度语义与 Token 使用趋势一致。
- 老日志在流量计数上线前无法补录流量；按现有应用层计数口径，不包含 TCP/TLS、HTTP Header、WS Frame 或重传开销。
- 旧 `/stats` 累计/今日字段仍留在 API，以免破坏既有调用方；只移除仪表盘旧卡片。

## 验证

- Go repository 趋势测试覆盖小时/日预聚合、带用户筛选的原始日志路径及四项字节映射。
- 前端 Vitest 覆盖图表五条曲线、双轴范围、空状态和卡片位置；执行 `vue-tsc --noEmit` 与构建。
- 在备用服务器发布后检查服务健康、`llmapi.thkl.site`、仪表盘筛选与流量趋势，以及关键上游连接。

## 发布与回滚

目标版本：`v0.2.9-traffic-trend-dashboard`；目标服务器：备用服务器 `cc.thkl.site`。关联开发分支：`codex/traffic-trend-dashboard`；功能提交：`ff2921af11448803295ca496ddc07bc8d60f0f71`，发布分支合并提交由发布包 manifest 记录。完整发布包及备用机脚本见 `tools/releases/traffic-trend-v029/`；该目录的组包器只接受干净的 `codex/release-sub2api` HEAD。发布前从备用服务器实际运行镜像、双 Compose、迁移及配置做基线预检；部署脚本在切换前备份数据库、运行镜像和配置。回滚保留业务数据库与流量日志，只恢复前一版镜像与配置；本功能无不可逆迁移。旧流量卡片随旧前端镜像恢复。
