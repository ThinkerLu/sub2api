# 使用记录的双向传输维度

目标版本：`v0.2.10-dual-leg-thinking-ws`（待发布）。关联提交为本功能分支的 `feat(usage): record ingress transport and expose HTTP to WS filters`。

## 目的和使用

使用记录新增独立 `ingress_transport`，0=未知、1=HTTP、2=WS。OpenAI 兼容 handler 在提交异步用量记录前明确入站协议；`upstream_transport` 继续按实际执行路径记录。两者不从 `request_type`、模型名或账号 WS mode 推断。

用户和管理员使用记录由两列派生 `HTTP → WS`、`WS → HTTP`、`WS`、`Stream/Sync` 标签，并用于筛选和前端 CSV 导出。新增 `HTTP → WS` 查询映射为 `ingress_transport=1&upstream_transport=2`，不新增数据库 request_type 枚举，也不隐式附加 stream 过滤。`cyber`/`live` 保持显示优先级。

旧记录的 0 值保持原有展示；旧 `WS → HTTP` 的 request_type/upstream_transport 筛选兼容路径保留。尚未显式捕获入站协议的其他内部记账路径仍为未知，不补造 HTTP 值。

## API、迁移与限制

- 使用记录 DTO 增加 `ingress_transport` 数字字段；列表、统计、仪表盘和 snapshot 接口接受 0/1/2 的可选同名筛选，非法值返回 400。
- 迁移 `242_usage_log_ingress_transport.sql` 增加非空 SMALLINT 列，默认 0。不会回填旧行，不改计费键或历史 request_type。
- 新字段贯穿同步/批量插入及扫描。筛选参与统计缓存键；按入站传输过滤时不使用缺少此维度的预聚合趋势表。
- 标签记录已发生的传输组合，不会因此自动开启 HTTP 入站转 WS 的新路由。

## 验证

repository、管理员查询、DTO、迁移完整测试通过；前端传输派生、语言键、使用记录页面/筛选组件共 67 项测试和 `vue-tsc --noEmit` 通过。新增断言覆盖四种组合、未知历史、业务类型优先级、两列精确筛选、缓存隔离和趋势绕过无维度聚合。

真实 PostgreSQL 演练：`SUB2API_POSTGRES_PROBE_CONTEXT=<本地 Docker context> go test ./internal/repository -run '^TestUsageIngressTransportPostgresRoundTrip$' -count=1`。测试创建独立临时 PostgreSQL 容器，验证迁移幂等、历史值为 0、新列单行/批量写入和扫描，以及旧程序列列表在迁移后仍可写入。测试容器自动移除，不使用生产数据。

## 发布与回滚

应用启动时执行新增迁移。发布包记录其文件及内容校验和，部署前备份数据库和迁移清单。回滚只允许本次已知的 additive 迁移差异，保留新列和已记录值；旧程序显式列列表继续读写，后续旧版写入的 ingress 值为 0。不得通过删除列或恢复旧数据库丢弃业务数据。

关联实现提交：`291434684`。
