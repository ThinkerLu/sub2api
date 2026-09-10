# HTTP、WS 桥接与客户端回退内存专项

2026-09-11；基线 `v0.2.4` / `5de5e2bed`，最终生产代码 `107759e11`。
沿用 `codex/memory-optimization-v024` worktree。本轮新增 **8 个独立优化提交**，连同第一轮共 19 个；未部署。

## 测试口径与真实案例

真实 `OpenAIGatewayHandler.Responses` / `ResponsesWebSocket`、兼容改写和转发逻辑运行在独立子进程。父进程充当客户端和本地模拟上游，父进程图片、序列化和哈希内存不计入网关。模拟账号不包含生产凭据，未调用实际模型。测试配置 bridge 阈值 4 MiB、首帧上限 64 MiB、HTTP 上限 128 MiB，未修改生产配置。

指定任务为 `codex://threads/01a08c38-70e1-7f23-ae04-d7131125e4a2`（准备车内告别剧情视频）。在其本地 rollout 中找到 35 个实际图片块。没有取得原始出站请求抓包，因此提供两种明确标注的重建负载：

- **图片+工具历史**：截取到第 14 个图片块的 response items，移除 reasoning 记录，保留其余消息和工具调用/输出，重建 Responses 请求封装。正文 **42,278,907 字节（40.32 MiB）**，图片 URL **42,058,700 字节，占 99.48%**。这是主案例；保留历史能暴露仅用图片消息无法命中的兼容处理开销。
- **图片消息对照**：相同 14 个实际图片块置于一个 user 消息中，正文 **42,059,736 字节（40.11 MiB）**，图片占 **99.998%**。用于区分历史兼容开销与图片本身开销。

这两者都不是线上完整请求的逐字回放，也不包含实际模型推理。没有将原始图片或私密历史提交到 Git；仅提交重建脚本、字节数、哈希及测量结果。`images-fixture.json` / `history-fixture.json` 记录来源边界和 SHA-256。

普通/图片消息场景校验 canonical input 哈希。工具历史原本就经过 ID/兼容清理，因此首先校验所有图片 URL 的顺序及哈希，再对比新旧版本实际发送到模拟上游的 **完整 canonical input 哈希**：三个场景、两种 GC 模式全部相同。没有以删掉图片或改变请求内容换取收益。

`ws-threshold-bridge` 走真实首帧阈值分流，断言上游 WS 握手次数为 0，HTTP 成功并返回 WS 完成事件。`ws-client-fallback` 先让实际上游 WS 握手返回 503，确认连接失败，随后客户端通过 HTTP 重发同一 input。没有新增网关内部“WS 失败自动转 HTTP”的业务行为。

## 优化策略、内存收益与风险

单项微基准为 8 MiB 负载、3 次运行的 B/op 中位数。它表示累计分配，不是常驻内存；单项收益不能与组合收益相加。

| 优化策略 | 内存收益 | 风险 | 提交 |
|---|---|---|---|
| HTTP bootstrap：没有候选 call output 时跳过全量解码 | 两项检查合计 **160.02 MiB → 0 B/op**（图片消息） | 低：疑似候选和 Unicode 转义走原有完整校验，重复键仍不改写 | `dd07ad9f2` |
| HTTP bootstrap：普通工具历史不含必需 envelope 标记时跳过解码 | 真实历史 HTTP 累计分配阶段对照约 **1238.54 → 787.99 MiB**；这是阶段组合差值，包含同期响应计数的小额变化 | 低：委派要求 codex_delegation，自动化要求 Automation 或 automation_id；转义保守回退 | `77b3193aa` |
| HTTP client tool 只读扫描借用正文 | **8.01 MiB → 0 B/op** | 低：递归扫描同步执行，视图不外泄，不修改正文 | `c7ecc4110` |
| HTTP input namespace：无改动先返回，只重建确需删除的项 | tools 有 namespace、input 无需改动：**24.02 MiB → 0 B/op** | 低–中：保留原来的 namespace、重复键和工具调用保留规则；有改动仍用原 sjson 删除 | `953466a9f` |
| WS 超阈值桥接：只改 envelope，一次写入最终正文 | **56.02 → 8.01 MiB/op**，减少约 85.7% | 中：JSON 字面形式不再全量规范化；保留顶层重复键末值、数字精度和 reasoning 规则，opaque input 值语义保持 | `d83ded94d` |
| WS 桥接：OpenAI 不创建只供 Grok 使用的正文快照 | 结构上减少一份完整正文复制，8 MiB 输入约省 8 MiB 分配 | 低：Grok 仍使用原独立快照和继承工具处理 | `f2c845d21` |
| HTTP JSON/SSE 图片计数借用正文，只独立保存 ID/尺寸 | 大文字 JSON 响应的两次计数 **16.02 MiB → 192 B/op** | 中：长期保存的 key、size 必须 Clone；覆盖源正文覆写后计数、尺寸及去重仍正确 | `16ea10a99` |
| HTTP 请求视图的 model/session 等小字段独立持有 | 40 MiB 图片消息单项对照：完成后 GC 堆留存 **64.51 → 0.27 MiB**，累计分配基本不变 | 低：新增少量小字符串分配，字段取值规则不变；防止转发结果/请求上下文挂住整包 | `107759e11` |

第一轮的 11 项和 passthrough 三轮结果见 [总报告](../REPORT.md)。

## 端到端组合收益

以下均为 MiB，累计分配和采样峰值来自自然 GC 场景；最后一列在处理完成后额外强制 GC，不能当作自然峰值。所有指标相对网关初始化后基线。峰值每 5 ms 采样，可能漏掉短峰值。

### 指定任务：实际图片与工具历史重建

| 链路 | 累计分配：原版 → 最终版 | 采样峰值堆增量 | 完成后 GC 留存增量 |
|---|---:|---:|---:|
| HTTP OAuth passthrough | 2187.69 → **787.99（-64.0%）** | 442.74 → 378.19 | 40.58 → 0.26 |
| WS 超阈值转 HTTP | 1874.18 → **742.60（-60.4%）** | 468.20 → 346.75 | 0.25 → 0.26 |
| WS 失败后客户端 HTTP 重发 | 3509.21 → **1449.53（-58.7%）** | 490.49 → 387.42 | 40.64 → 0.31 |

### 相同实际图片、无工具历史对照

| 链路 | 累计分配：原版 → 最终版 | 采样峰值堆增量 | 完成后 GC 留存增量 |
|---|---:|---:|---:|
| HTTP OAuth passthrough | 1770.40 → **288.49（-83.7%）** | 405.94 → 216.86 | 64.27 → 0.26 |
| WS 超阈值转 HTTP | 1569.28 → **242.84（-84.5%）** | 557.59 → 161.00 | 0.25 → 0.27 |
| WS 失败后客户端 HTTP 重发 | 2798.19 → **450.72（-83.9%）** | 405.99 → 216.90 | 64.31 → 0.31 |

### 合成负载覆盖矩阵

请求场景为 8 MiB 单图片；大响应场景为 1 KiB 请求、8 MiB 文字响应。`oauth-normal` 关闭账号 passthrough，覆盖普通 HTTP 适配。

| 链路 | 累计分配：原版 → 最终版 | 采样峰值堆增量 | 完成后 GC 留存增量 |
|---|---:|---:|---:|
| HTTP OAuth passthrough | 455.73 → 47.61（-89.6%） | 104.15 → 40.20 | 16.26 → 0.26 |
| HTTP APIKey passthrough | 447.73 → 127.64（-71.5%） | 88.13 → 64.15 | 16.27 → 0.26 |
| HTTP OAuth 普通适配 | 399.74 → 111.69（-72.1%） | 88.16 → 56.19 | 16.27 → 0.27 |
| HTTP 非流式大响应 | 32.80 → 16.72（-49.0%） | 24.15 → 8.45 | 0.27 → 0.26 |
| HTTP SSE 大响应 | 80.49 → 72.43（-10.0%） | 56.41 → 48.40 | 0.26 → 0.26 |
| HTTP 取消 | 455.75 → 47.62（-89.6%） | 104.15 → 40.20 | 16.26 → 0.25 |
| WS 超阈值转 HTTP | 297.03 → 40.88（-86.2%） | 96.58 → 32.55 | 0.25 → 0.27 |
| WS 失败后客户端 HTTP 重发 | 672.36 → 72.11（-89.3%） | 96.20 → 32.23 | 16.31 → 0.31 |

每种负载/版本分别运行 natural 和 forensic；共保存 **56 份端到端结果**。它们是本机单次场景对照，不是生产 RSS/并发容量承诺；GC、池复用、调度会影响数值。自然峰值与累计分配降幅不同，特别是工具历史仍有明显瞬时工作集。未改变压缩、并发、超时或生产请求限额。

## 剩余热点与取舍

最终真实历史 HTTP profile 中，兼容性正规化及 OAuth 保留工具名处理仍各有约 169 MiB 分配，input item ID 清理约 161 MiB，正文读取约 128 MiB。这些包含真实历史上的改写/完整校验，当前没有直接删掉。后续可继续做保留重复键和转义语义的局部重建，但本轮不把未经验证的潜在收益计入结果。

大 SSE 响应仍有扫描和转发缓冲开销，收益仅约 10%，没有把小请求或非流式收益套用到该场景。WS 回退总分配包含失败 WS 和随后的 HTTP 两次尝试，并非只统计成功重试。完成后少量固定留存属于服务、连接和计量基础设施；本轮未声称清零。

## 验证与复现

Go 1.27.0，darwin/arm64，Apple M2 Pro。测试覆盖 bootstrap 边界、转义、历史与重复键；bridge 大整数/重复字段/null/非法 JSON/独立缓冲；计数元数据所有权；request view 小字段所有权；既有 namespace、Grok、passthrough 和回退测试。

`results/` 保存逐场景原始 JSON 和工具历史的上游哈希；`summary.json` 汇总新旧指标，`bench-before.txt` / `bench-after.txt` 为三次微基准原始结果。完整回归和竞态结果见 `verification/`。

在主 worktree 执行：

```sh
python3 tools/diagnostics/memory-optimization/run_http.py --root ../memory-baseline-v024 --output /tmp/http-before --go /path/to/go --cases oauth-stream,apikey-stream,oauth-normal,nonstream-response,stream-response,cancel,ws-threshold-bridge,ws-client-fallback
python3 tools/diagnostics/memory-optimization/run_http.py --root . --output /tmp/http-after --go /path/to/go --cases oauth-stream,apikey-stream,oauth-normal,nonstream-response,stream-response,cancel,ws-threshold-bridge,ws-client-fallback
python3 tools/diagnostics/memory-optimization/build_image_fixture.py /path/to/rollout.jsonl /tmp/history.json --history --images 14
python3 tools/diagnostics/memory-optimization/run_http.py --root . --output /tmp/http-history --go /path/to/go --payload /tmp/history.json --history --cases oauth-stream,ws-threshold-bridge,ws-client-fallback
python3 tools/diagnostics/memory-optimization/run_http_benchmarks.py --root . --go /path/to/go --output /tmp/http-bench
```

`--history` 将图片顺序哈希作为单次回放断言，并额外保存完整上游 input 哈希，须跨版本比较后再认定兼容性一致。首次直接用原 input 哈希校验工具历史会因既有清理逻辑报差异；已修正诊断方式，不把该现象误报为本轮产品缺陷。
