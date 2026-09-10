# sub2api v0.2.4 内存优化专项

日期：2026-09-11。基线为原始 `v0.2.4` / `5de5e2bed`，生产代码最终版本为 `0fcff2b03`。
工作分支：`codex/memory-optimization-v024`；工作目录：`sub2api/memory-optimization-v024`。
11 项优化分别提交，另有基准和报告提交；未合并其他分支、未部署服务器。

WS 主线明确为 **passthrough**。桥接与历史重放属于补充优化，不能据其微基准推断 passthrough 收益。

## 真实 passthrough 结果

真实 `ResponsesWebSocket` handler、OAuth passthrough adapter、coder/websocket 和 relay 在独立网关子进程运行；父进程运行客户端和本地模拟上游，客户端/上游的内存不计入网关。关闭 HTTP bridge，明确启用 passthrough，协商 WS 压缩。首轮为 8 MiB 图片输入，随后两轮为小增量；每轮校验到达上游的 input 哈希和完成事件。

| 指标 | v0.2.4 | 优化后 | 降幅 |
|---|---:|---:|---:|
| 累计分配（自然 GC） | 174.92 MiB | 30.72 MiB | 82.4% |
| 采样峰值堆增量（自然 GC） | 64.19 MiB | 20.51 MiB | 68.1% |
| 首轮流式等待留存（强制 GC） | 16.98 MiB | 8.99 MiB | 47.1% |
| 首轮完成空闲留存（强制 GC） | 9.84 MiB | 1.84 MiB | 81.3% |
| 三轮后空闲留存（强制 GC） | 9.84 MiB | 1.84 MiB | 81.3% |

自然 GC 和强制 GC 是两次独立场景，不把强制 GC 的低留存当自然峰值。所有增量均相对网关初始化后的基线；峰值每 5 ms 采样，可能漏掉极短峰值。关闭连接后强制 GC，两版均仅剩约 0.10 MiB 增量，主要改善的是请求过程中和长连接空闲期间的内存。
这是本机合成负载的单组端到端对照，不是生产 RSS、并发容量或 OOM 概率的承诺。微基准每项运行 3 次取中位数；各项收益不可直接相加，部分检查串联在同一条路径。

## 优化策略、内存收益与风险

除标明端到端或结构推导的项目外，以下为 8 MiB 负载的单次操作分配量 `B/op`（MiB = 1,048,576 字节），不是峰值常驻内存。

| 优化策略 | 内存收益 | 风险 | 独立提交 |
|---|---|---|---|
| **passthrough：身份字段合并改写**，只解析小 metadata，单次生成最终请求 | 24.03 → 8.01 MiB，约 **66.7%** | 低–中：必须保持账号隔离值、重复键首值和转义键语义；已做身份与原始 JSON 对照测试 | `5961f698a` |
| **passthrough：兼容检查无改动快路径**，原生 ingress、reasoning、item ID、孤立工具输出先只读扫描 | 152.08 → 约 40.02 MiB；单独 compaction 项完成后，整条兼容检查降至 <1 KiB | 中：不能跳过孤立输出清理、旧格式转换或错误处理；模糊情形退回原解码路径，重复键和 previous_response_id 有专门测试 | `9f311b5e8` |
| **passthrough：共享不可变入口正文，首轮成功后释放重试引用** | 结构上消除风控/初始重试的两份整包克隆（8 MiB 首帧少复制约 16 MiB）；组合实测首轮后空闲留存少约 **8 MiB** | 中：依赖正文不可变约定；后续失败必须携带当前轮重试正文，锁保护清理与失败重试 | `e740c49db` |
| **passthrough：无 compaction 标记时跳过整包解码** | 40.02 MiB → 接近零分配；包含在最终兼容链收益内 | 低：遇到标记文本或 Unicode 转义仍走原路径；无效 JSON 保留原错误路径 | `9b5793d4c` |
| **WS/HTTP：模型准入候选提取只复制最终模型名** | 16.02 MiB → <1 KiB，且返回模型名不再引用整包 | 中：模型准入必须保留重复键、大小写变体、session 与 Live 路由候选；同步视图不外泄，返回字符串独立持有 | `0fcff2b03` |
| HTTP：读缓冲区预留 EOF 探测空间，保持原 1 MiB 预分配上限 | 8 MiB 请求：31.00 → 15.06 MiB（**51.4%**）；1 MiB：3.00 → 1.01；16 MiB：63.00 → 31.13 | 低：增加 512 字节容量提示，分配器可能按页取整；收益依赖正文大小和 reader 行为，未知长度不保证同幅收益 | `f26990794` |
| HTTP：图片输入的只读检查复用 JSON 视图 | 两次检查合计 16.02 MiB → 接近零分配 | 低：只在同步只读检查内借用；复杂转义文本仍可能由解析器分配字符串 | `0a7eb4c20` |
| HTTP/Grok：删除非原地模型改写前的冗余克隆 | 168.11 → 160.10 MiB（**4.8%**），稳定少一份约 8 MiB 请求副本 | 低：依赖固定版本 sjson 默认 `ReplaceInPlace=false`；测试覆盖输出改写后原请求保持不变 | `aa10db958` |
| WS 桥接补充：取 tools 小字段时只复制目标字段 | 8.01 MiB → <1 KiB | 低：保留独立 tools 存储，避免小字段挂住大 input；完整 JSON 校验、重复键末值语义有测试 | `f1ed0d825` |
| WS 重放补充：历史直接写入最终 payload | 17.88 → 8.01 MiB（**55.2%**） | 中：JSON 文本的空白/HTML 转义形式可能保留原样；值语义、大整数、nil 与空数组、重复键替换有对照测试 | `8ebae1046` |
| WS 重放补充：按不可变 JSON 视图比较历史 | 键顺序变化的 8 MiB 历史：105.64 MiB → <1 KiB | 中：保留数字字面量、重复键末值及数组顺序；非法 UTF-8 回退旧实现，巨大转义字符串未保证零分配 | `d58165ada` |

## 排查与未采用方案

passthrough 的 relay pump 本身未发现额外的整帧克隆；大分配主要位于入口模型候选提取、兼容性处理、身份字段改写及 handler 的风控/重试留存。保留完整帧、压缩协议、读限额、重试和计费功能，未使用丢弃历史或降低请求限额来制造收益。

尝试过用倍增缓冲替换 `coder/websocket.Read` 的累积读取，但**未采用、已撤回生产改动**：当前 Go 1.27 的 `io.ReadAll` 已是分块读取后最终拼接，不能套用旧版连续扩容假设。8 MiB 帧仅从约 16.27 → 16.00 MiB；8 MiB+1 字节却从约 16.28 → 40.01 MiB，12 MiB 从约 24.38 → 44.00 MiB，均出现反向优化。实验源码以 `.go.txt` 留在 `rejected-frame-reader/`，不会编译进后端；证据见 `frame-read-benchmark.txt`。

本轮没有改动压缩算法、全局缓存上限或并发内存准入。这些可能改变 CPU、命中率、请求排队或拒绝行为，需要不同的负载模型，不混入复制优化收益。

## 验证与复现

Go 1.27.0，darwin/arm64，Apple M2 Pro。基准 fixture 在计时外构造，使用生产函数；新旧版覆盖相同测试文件，`-benchmem -count=3 -benchtime=200ms`。原始数据在 `before.txt` / `after.txt`，中位数在 `benchmark-summary.json`。兼容性独立阶段见 `compat-stage.txt`（`9f311b5e8`）。

真实网络结果：`passthrough-before-natural.json` / `passthrough-after-natural.json` 及对应 `forensic` 文件。各轮 input 哈希校验由诊断测试执行。网络基线的源码来自本项目既有 passthrough 诊断，并随本报告保存。

测试汇总见 `verification.json`。普通构建执行分配预算断言；race 构建保留语义、所有权和并发检查，不执行受插桩影响的三项字节预算断言。初次 race 的两个失败均为分配预算超限，没有数据竞争报告，调整测试分组后重新运行。覆盖 service、handler、HTTP 正文工具、模型候选提取和 WS v2 relay；竞态检查覆盖新增优化、passthrough 生命周期、首帧读取/取消、首轮与当前轮失败重试、JSON 兼容和 relay。

在主 worktree 执行（需要 Go 1.27；若不在 PATH，通过 `--go /absolute/path/to/go` 指定）：

```sh
python3 tools/diagnostics/memory-optimization/run_benchmarks.py --root ../memory-baseline-v024 --output /tmp/sub2api-before.txt
python3 tools/diagnostics/memory-optimization/run_benchmarks.py --root . --output /tmp/sub2api-after.txt
python3 tools/diagnostics/memory-optimization/run_network.py --root ../memory-baseline-v024 --output /tmp/sub2api-network-before
python3 tools/diagnostics/memory-optimization/run_network.py --root . --output /tmp/sub2api-network-after
cd backend
go test ./internal/service ./internal/handler ./internal/pkg/httputil ./internal/pkg/requestmodel ./internal/service/openai_ws_v2 -count=1
```

测试使用本地假账号和模拟上游，不需要生产凭据。每项提交可单独审查；整体网络收益对应全部优化组合，补充重放项不归入 passthrough 的因果收益。
