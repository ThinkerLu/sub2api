from pathlib import Path
import hashlib
import json
import shutil
import subprocess


HERE = Path(__file__).resolve().parent
REPO = Path('/Users/thinkerlu/Documents/apiProxy/sub2api/ws-adaptive-http-bridge')
manifest = json.loads((HERE / 'image' / 'manifest.json').read_text())
delivery = HERE / 'delivery'
delivery.mkdir(exist_ok=True)

for path in HERE.iterdir():
    if path.is_file() and path.name not in ('.gitignore', 'package_delivery.py'):
        shutil.copy2(path, delivery / path.name)

for name in (
    manifest['archive'],
    'go-build-info.txt',
    'linux-amd64-regression.log',
    'linux-amd64-adaptive-regression.log',
    'image-version.txt',
    'smoke-http.txt',
    'frontend-build.log',
    'backend-build.log',
    'image-build.log',
):
    shutil.copy2(HERE / 'image' / name, delivery / name)

shutil.copy2(HERE / 'image' / 'model-registry.example.yaml', delivery / 'model-registry.example.yaml')

manifest.update(
    branch='codex/ws-adaptive-http-bridge',
    rollback_commit='17866c78b682f4cd675404fb4665a868016038c3',
    rollback_image='sub2api:v0.2.5-ws-preflight-fallback-17866c78b',
    scope='Read the effective model from Codex x-codex-turn-metadata before WebSocket upgrade so non-WS models return HTTP 426 immediately',
    database_migrations_changed=False,
    rollback_preserves_adaptive_modes=True,
    rollback_preserves_model_registry=True,
    remote_directory='/home/thinkerlu/sub2api-releases/ws-turn-metadata-fallback-v025-' + manifest['commit'][:9],
)
(delivery / 'manifest.json').write_text(json.dumps(manifest, indent=2) + '\n')

for source, name in (
    ('sub2api-ws-turn-metadata-regression.log', 'regression-race.log'),
    ('sub2api-ws-turn-metadata-release-tests.log', 'release-tests.log'),
):
    optional = Path('/tmp') / source
    if optional.is_file():
        shutil.copy2(optional, delivery / name)

shutil.copy2(HERE / 'image' / 'linux-amd64-adaptive-regression.log', delivery / 'linux-amd64-preflight-regression.log')

shutil.copy2(HERE / 'README.md', delivery / 'IMPLEMENTATION.md')

with (delivery / 'source.patch').open('wb') as output:
    subprocess.run(
        ['git', 'diff', '--binary', manifest['base_commit'], manifest['commit']],
        cwd=REPO,
        stdout=output,
        check=True,
    )

subprocess.run(
    [
        'git',
        'bundle',
        'create',
        str(delivery / 'source.bundle'),
        'v0.2.5..refs/heads/codex/ws-adaptive-http-bridge',
    ],
    cwd=REPO,
    check=True,
)

(delivery / 'commits.txt').write_text(
    subprocess.check_output(
        ['git', 'log', '--oneline', manifest['base_commit'] + '..' + manifest['commit']],
        cwd=REPO,
        text=True,
    )
)

(delivery / 'VALIDATION.md').write_text(
    '''# 可重复验证

在源码 backend 目录，使用 Go 1.27：

```sh
go test -race ./internal/service ./internal/handler -run 'TestOpenAIWSTurnMetadataModel|TestOpenAIResponsesWebSocket_NoNativeWSModelRouteReturnsUpgradeRequired|TestOpenAIWSModelRouteAvailable' -count=1
go test ./internal/service ./internal/handler -count=1
```

在发布目录：

```sh
python3 -m unittest discover -s . -p test_release.py -v
sha256sum -c SHA256SUMS
sudo python3 release.py preflight
```

前端从本包提交重新构建，包含类型与 i18n 检查。Linux amd64 镜像内运行真实 WebSocket relay、adaptive 和新增 turn-metadata 预检回归，并检查内嵌前端启动。测试使用本地模拟上游，没有调用真实模型。

部署后验收：Codex 连接 `deepseek-flash` 时，WS 升级前应收到 HTTP 426，日志中不得出现 `Reconnecting... 1/5` 到 `5/5`；随后 HTTP 请求应由账号 3 langbase 成功处理。

上传不执行镜像导入、完整线上 preflight 或服务切换。线上 preflight 需要管理员 sudo 密码。回滚恢复 `17866c78b` 实际容器快照，保留数据库、data、v0.2.5 迁移记录和外置 model registry。
'''
)

lines = []
for path in sorted(delivery.iterdir()):
    if path.is_file() and path.name != 'SHA256SUMS':
        digest = hashlib.file_digest(path.open('rb'), 'sha256').hexdigest()
        lines.append(digest + '  ' + path.name + '\n')
(delivery / 'SHA256SUMS').write_text(''.join(lines))

print(manifest['remote_directory'])
print(len(lines), 'files')
