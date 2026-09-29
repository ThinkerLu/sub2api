# v0.2.9 发布工具压缩旧镜像备份

目标版本：`v0.2.9-beta1-ws-standalone-named-output`，交付工具修订 `r1`。应用镜像和源码提交保持 beta1 不变。

## 目的

beta1 首次部署在主服务器备份旧镜像时失败：脚本用 `docker save -o old-image.tar` 写入未压缩归档，主服务器剩余空间不足。虽然服务仍运行旧镜像且 Compose 未切换，但发布无法继续。`r1` 保留同一个应用镜像，把旧镜像备份改为压缩流，并清理未完成备份。

## 关键行为

- 旧镜像备份改为 `docker save <旧镜像> | gzip -1 > old-image.tar.gz`，直接写压缩归档，不再先写接近镜像虚拟大小的未压缩 tar。
- 写入后执行 `gzip -t`，摘要仍记录在 `backup.json` 的 `old_image_sha256`。
- 回滚使用 `gzip -dc old-image.tar.gz | docker load` 恢复旧镜像。
- 部署前扫描同一发布前缀的备份目录；没有 `backup.json` 且未被当前发布指针引用的目录视为未完成目录并清理。
- 部署失败且尚未写发布指针时，也会清理本次未完成目录。已写指针或已有完整 `backup.json` 的备份不会被自动删除。
- 应用镜像、数据库、Compose 切换、恢复窗口排空和健康检查逻辑不变。

## 配置和数据

无新增运行配置、API 或数据库迁移。压缩归档仍保存在目标机私有备份目录，不进入 Git 或聊天记录。

## 验证

```sh
python3 -m unittest -v test_release.py
```

新增测试覆盖压缩备份管道、gzip 完整性检查，以及只清理“同前缀、无 backup.json、未被指针引用”的未完成目录。应用镜像未重建，因此沿用 beta1 已完成的后端、前端和镜像冒烟结果。

## 发布与回滚

`r1` 作为 beta1 的交付工具修订重新组包，manifest 记录 `delivery_revision=r1`。先上传小体积修订包并校验 SHA256，再在主服务器执行预检和部署。回滚恢复 beta1 的上一 beta3 应用镜像；无数据库迁移。
