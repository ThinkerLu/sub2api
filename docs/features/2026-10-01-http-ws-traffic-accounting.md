# HTTP→WS 上游流量统计修复

首次部署：`v0.2.11-probe19-http-ws-traffic`。2026-10-02 从已部署发布包的冻结源码迁入 `codex/cc-probe18-20-import`，仅供备用机发布分支，不直接合入正式发布分支。

## 行为与使用

HTTP 入站经 WSv2 转发时，原先用量记录漏记上游请求和响应字节。本修复在请求 context 设置专用计数器，成功写入文本消息后计入序列化字节，读取原始消息后计入响应字节。流式、非流式和合并 JSON 消息使用同一计数。

包含 JSON 编码换行、预热与实际重试的消息，不包含握手、WS 帧及控制帧。后台建连不归属业务流量；WS 入站继续使用逐轮计数，避免重复。后台用量明细自动显示新请求统计，历史零值不估算回填。

无需新增配置、API 或数据库迁移。

## 验证与回退

四组回归逐字节核对 WS 消息长度（流式/非流式 × 分条/合并）；使用 `tools/go.sh` 运行 WS forwarder、UsageTraffic、OpenAIWSConn 和 CodexV2 测试及定向 race 检查。

原 probe19 部署验收通过，证据见工程顶层 `orchestration/2026-10-01-http-ws-traffic/acceptance.json`。冻结源码摘要由发布包 `build.json` 与 `source-files.json` 记录；本次关联提交为包含本文档的功能提交。

部署使用备用机完整原生包及原子更新工具。probe19 直接回退来源为 probe18，不恢复数据库。本次仅迁入代码，不重新部署。
