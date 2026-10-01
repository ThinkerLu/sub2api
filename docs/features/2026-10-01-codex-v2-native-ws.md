# Codex V2 原生 WS 命名空间替换

## 发布分支同步（probe18）

按用户要求将 `codex/release-sub2api` 的 `60a84dbbd` 合入本开发分支。
备用机候选版本为 `v0.2.11-probe18-codex-v2-native-ws`，仍是原生探针，不反向合入发布分支。
已集成的官方来源为 `v0.2.11`（`96f4c115c9749078f90cbf210a01d39baf3f53b6`）。
与 probe17 相比，SQL 迁移文件没有变化；部署工具仍须核对目标数据库的迁移摘要，
发现变化即停止，不能据版本号自行运行数据库升级。回退来源为原生 probe17，不恢复数据库。
合并保留上游 WS 错误码透传和 Codex V2 回程改写。验证范围包括后端全量测试、
原生 WS 与双链路 race 测试、前端全量测试/类型检查/嵌入资源构建及部署后真实父子双轮探针。
实际构建来源、摘要和部署结果以 probe18 包的 `build.json` 与验收记录为准。

## 范围与来源

目标为备用机原生服务；探针版本 `v0.2.8-probe17-codex-v2-native-ws`，不合入发布分支。
用户已授权提交，不推送。开发分支 `codex/codex-v2-native-ws` 从发布分支提交 `c4586c46a` 创建，
移入已验证的 probe16 分组兼容代码，再补齐原生 WS。正式集成的 tag 仍以 `v0.2.8` 为基线；
源码 VERSION 文件的 0.2.9 不作为自行升级官方基线的依据。manifest 分别记录各项来源。

此前启用分组的原生 WS 被临时守卫拒绝。现在在直通实际写帧处、连接池每轮发送处和原生
回程写出处执行命名空间替换、明文消息转换、工具身份还原及完整参数明文标记。
`ctx_pool`、`shared`、`dedicated`、`passthrough`、adaptive 的 WS 分支共用连接级策略。
HTTP→WS 的回程也接入还原。保留 forced OpenAI Chat Completions 的独立拒绝条件。

## 配置与安全边界

继续使用 `codex_v2_group_policy.file`，仅 GroupID 3 启用。连接首次固定分组策略，重载影响
新连接；已有连接不会半途切换协议行为。原始顶层/Lite 声明按连接保存，省略声明时重新推导
上游别名；显式空声明清除来源。adaptive HTTP/WS 切换共享原始来源，不把已改写 schema
当作客户端声明。请求写入和响应读取使用分离的状态，标记以原子变量发布。

真实密文、别名冲突、未知消息片段仍在出站前拒绝。delta/空参数不补明文标记。
非 response.create 控制帧保持原行为；response.steer 的明文 agent_message 支持转换。
session.update 更新后续声明时不篡改当前响应的标记。未启用分组逐字节保持原行为。

无数据库结构改动；源码全部迁移与 probe16 相同。更新前后核对实际迁移清单，保留上一份
程序、私有环境和元数据；回退不恢复数据库，不启动旧 Docker 应用。

## 验证与交付

Go 回归覆盖首帧/续帧、两种声明位置、显式清空、密文拒绝、原生四种模式及 adaptive 两条
链路，并运行 race 检查。真实 CLI 验收使用同一子代理双轮，每轮读取一个轻量文件并执行
工具，父代理等待结果再用 followup_task 恢复该子代理。五组模型与 probe16 一致。

本机直接域名 WS 遇到证书名称不匹配，不能关闭 TLS 校验。可通过已验证 SSH 主机的加密
通道连接备用机回环服务；这仍实际使用 WS，但必须明确入口不同。验收必须核对数据库记录
中的 ingress_transport=2、upstream_transport=2，并拒绝 CLI 静默 HTTP 回退。

## 2026-10-01 验收结果

probe17 已部署，GroupID 3 保持启用。五组均通过：DeepSeek→GPT、DeepSeek→DeepSeek、
GPT→DeepSeek、GPT→GLM、GPT→GPT。实际模型为 deepseek-flash、gpt-6-luna、glm-5.3-flash；
Codex CLI 0.159.2，每组只有一个子线程，followup_task 恢复同一线程进行第二轮，每轮都有
成功的文件读取工具调用，父代理收到两次结果，没有 HTTP 回退。

专用测试 key 的限定时间窗内，56 条用量记录全部为 WS 入站、WS 上游（2/2），且上游返回
模型字段匹配；DeepSeek 22、GPT 29、GLM 5。测试经带保活的已验证 SSH 通道连接真实备用机
原生服务，未使用本地协议改写代理。公网域名直连 WS 在本机仍遇到 DNS/TLS 路由错误，
因此不把本结果扩大为“本机公网域名直连已验证”。证书校验未关闭，服务器 TLS 配置未改动。

本机冻结源码上 132 项定向测试通过，无 skip；原生模式及状态处理的 race 检查通过，
原生更新工具 26 项测试通过。HTTP 入站的 GPT→DeepSeek 双轮回归也通过。
证据：apiProxy 根目录 `orchestration/2026-10-01-codex-v2-native-ws/matrix-results.json`
及 `upstream-verification.json`；原始 CLI/rollout 只保存在本机私有目录。

二进制 SHA256：`7b33c24ebc484bd9ef6469d4452d4693b5d01c496712b5561ba5f8f266c5cc07`。
冻结源码清单 SHA256：`490f8557f433cff0f3ca8817bcd5039ec60792069bf276eff59dec2139a142c6`。
运行目录 `/home/thinkerlu/sub2api-native/releases/v0.2.8-probe17-codex-v2-native-ws/`。
回到上一份原生 probe16：

```sh
/home/thinkerlu/sub2api-native/releases/v0.2.8-probe17-codex-v2-native-ws/rollback.sh
```

无需 sudo，不恢复数据库，不切回 Docker。原程序和私有环境/元数据备份保留。
更新使用 rsync 候选临时文件后原子替换；首次 --inplace 差分验证失败，未部署该失败文件，
临时文件方式重传后另行核对 SHA256 才切换。不要对运行程序做 --inplace 覆盖。
