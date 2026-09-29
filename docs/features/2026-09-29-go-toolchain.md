# 持久 Go 工具链

日期：2026-09-29。开发分支 `codex/dual-leg-http-completion`，基于发布分支 `f7e0f0f62`。工具修订 r1，不改变应用版本，不涉及服务端部署或数据迁移。

实现提交：`ef00429df`；随 `690f49432` 合入 `codex/release-sub2api`。

## 安装与使用

Go 版本读取 `backend/go.mod`，当前为 1.27.0。`tools/install-go.sh` 从 Go 官方下载 JSON 清单获取本机架构的归档和 SHA256，验证后在 `$HOME/.local/share/go/<version>` 安装。已存在的安装只校验和运行标准库测试，不覆盖；损坏安装直接报错。

```sh
tools/install-go.sh
cd backend
../tools/go.sh test -p 2 -tags=unit ./... -count=1
```

`tools/go.sh` 检查编译器及 runtime/bytes 标准库文件，指定 GOROOT、GOTOOLCHAIN=local。默认构建缓存为 `$HOME/Library/Caches/sub2api-go-build`，模块缓存沿用 Go 默认 `$HOME/go/pkg/mod`。环境变量 SUB2API_GOROOT 可用于显式选择持久工具链；缺失目录必须失败，不允许静默切换临时目录或下载其他编译器。

本机 `.local/bin/go` 与 `gofmt` 已链接到持久 1.27.0，PATH 已包含该目录；未修改 shell 配置。打包脚本 `ws-standalone-v029-beta1-main/build_package.py` 默认使用 tools/go.sh。历史发布脚本不追改包，重新执行旧脚本时须显式提供 SUB2API_GO 和 GOROOT，见顶层 AGENTS.md。

## 来源与验证

官方清单：<https://go.dev/dl/?mode=json&include=all>。安装包 `go1.27.0.darwin-arm64.tar.gz` SHA256：`90493b3bbd5e10f91d12153198bf1994fd756399b4fec93b49b0c6e2acdeeb3e`，已与清单匹配。

已验证 go version、bytes/archive/tar 标准库测试，以及不存在的 SUB2API_GOROOT 返回非零。项目编译和测试结果另见 `2026-09-29-dual-leg-verification.md`。

## 维护与回滚

升级先安装新版本目录并核验官方摘要，不覆盖在用版本；在 go.mod 实际升级后才切换项目工具链。缓存可清理，工具链不是缓存，不应放入 /tmp 或随缓存清理。若回到旧源码，安装该源码 go.mod 指定的版本即可，不改线上镜像或数据库。
