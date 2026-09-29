#!/usr/bin/env python3
"""Build the v0.2.9 beta1 main-server image and complete delivery package."""

import datetime
import gzip
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import tarfile
import tempfile
import time


HERE = Path(__file__).resolve().parent
REPO = HERE.parents[2]
UPSTREAM_TAG = 'v0.2.9'
UPSTREAM_COMMIT = '4c00df2e0183e2c70b7fa8ba45914205e36aad0c'
UPSTREAM_TREE = '212ecc21a885afd84eb9ff70df2bf5d543b864eb'
VERSION = 'v0.2.9-beta1-ws-standalone-named-output'
BASE_IMAGE = 'sub2api:v0.2.8-beta3-adaptive-ws-keepalive-b3a100e05'
BASE_COMMIT = 'b3a100e05054b265a2e0a8a71827c659553503e9'
BASE_ID = 'sha256:84f6a16649c84907f523d55821d07c5903422565e6cbfe97c0c2ceac41b21ad6'
BASE_CONFIG_ID = 'sha256:24f939378b80e8a1f24bfa0d01a1a87d896441417f2c10f35966640708d10e21'
BASE_BINARY_SHA256 = '3493620575780e86f888ae5dad02da04f2fa8424ae8a1ca835eb0dac906a1943'
GO = os.environ.get('SUB2API_GO', '/private/tmp/go1.27.0/bin/go')
NODE = os.environ.get('SUB2API_NODE_BIN', '/Users/thinkerlu/.cache/codex-runtimes/codex-primary-runtime/dependencies/node/bin')
DOCKER = ['docker', '--context', 'colima-sub2api-build']
ENV = {**os.environ, 'PATH': NODE + ':/opt/homebrew/bin:' + os.environ.get('PATH', '')}
GO_ENV = {**ENV, 'CGO_ENABLED': '0', 'GOOS': 'linux', 'GOARCH': 'amd64',
          'GOTOOLCHAIN': 'local', 'GOCACHE': '/private/tmp/sub2api-v029-go-cache'}
TEST_ENV = {key: value for key, value in ENV.items() if key not in ('GOOS', 'GOARCH', 'CGO_ENABLED')}
TEST_ENV.update(GOTOOLCHAIN='local', GOCACHE='/private/tmp/sub2api-v029-go-cache')


def run(args, cwd, log=None, env=ENV):
    result = subprocess.run([str(arg) for arg in args], cwd=cwd, env=env,
                            capture_output=True, text=True)
    output = result.stdout + result.stderr
    if log:
        log.write_text(output)
    if result.returncode:
        raise RuntimeError('构建命令失败: ' + ' '.join(str(arg) for arg in args[:4]) +
                           ('；见 ' + str(log) if log else ''))
    return output.strip()


def sha(path):
    digest = hashlib.sha256()
    with Path(path).open('rb') as source:
        for block in iter(lambda: source.read(1024 * 1024), b''):
            digest.update(block)
    return digest.hexdigest()


def main():
    assert run(['git', 'symbolic-ref', '--short', 'HEAD'], REPO) == 'codex/release-sub2api', '只允许发布分支打包'
    assert not run(['git', 'status', '--porcelain'], REPO), '发布 worktree 必须干净'
    commit = run(['git', 'rev-parse', 'HEAD'], REPO)
    assert UPSTREAM_TREE in run(['git', 'log', '--format=%T', commit], REPO).splitlines(), '发布提交未包含上游 v0.2.9 tree'
    base = json.loads(run(DOCKER + ['image', 'inspect', BASE_IMAGE], REPO))[0]
    assert base['Id'] == BASE_ID and (base['Os'], base['Architecture']) == ('linux', 'amd64'), '回滚镜像摘要或平台不符'

    image = 'sub2api:' + VERSION + '-' + commit[:9]
    out = HERE / 'image'
    delivery = HERE / 'delivery'
    assert not out.exists() and not delivery.exists(), '已有构建产物，请人工检查后选择新目录'
    out.mkdir()
    delivery.mkdir()
    (delivery / 'checks').mkdir()
    with tempfile.TemporaryDirectory(prefix='sub2api-v029-beta1-build-', dir='/private/tmp') as tmp:
        work = Path(tmp)
        source_tar = work / 'source.tar'
        run(['git', 'archive', '--format=tar', '-o', source_tar, commit], REPO)
        source = work / 'source'
        source.mkdir()
        with tarfile.open(source_tar) as archive:
            archive.extractall(source, filter='data')
        dist = source / 'backend/internal/web/dist'
        if dist.exists():
            shutil.rmtree(dist)
        dependencies = REPO / 'frontend/node_modules'
        if not dependencies.is_dir():
            dependencies = REPO.parent / 'request-traffic-monitoring/frontend/node_modules'
        assert dependencies.is_dir(), '缺少已安装前端依赖'
        shutil.copytree(dependencies, source / 'frontend/node_modules', symlinks=True)
        frontend = source / 'frontend'
        frontend_tests = [
            'src/i18n/__tests__/localeKeyCompleteness.spec.ts',
            'src/views/admin/__tests__/DashboardView.spec.ts',
            'src/components/charts/__tests__/TrafficUsageTrend.spec.ts',
            'src/components/account/__tests__/UsageProgressBar.spec.ts',
            'src/components/admin/group/__tests__/GroupModal.cleanup.spec.ts',
            'src/components/modelPlaza/__tests__/PlazaGroupSection.spec.ts',
            'src/components/modelPlaza/__tests__/PlazaModelPricingTable.spec.ts',
            'src/utils/__tests__/ccswitchImport.spec.ts',
        ]
        run(['./node_modules/.bin/vitest', 'run', *frontend_tests], frontend,
            delivery / 'checks/frontend-tests.log')
        run(['./node_modules/.bin/vue-tsc', '--noEmit'], frontend,
            delivery / 'checks/frontend-typecheck.log')
        run(['./node_modules/.bin/vite', 'build'], frontend,
            delivery / 'checks/frontend-build.log')
        assert (dist / 'index.html').is_file(), '前端内嵌资源未生成'

        backend = source / 'backend'
        run([GO, 'test', '-tags=unit', './internal/service', './internal/service/openai_ws_v2', './internal/handler',
             '-count=1'], backend, delivery / 'checks/backend-tests.log', TEST_ENV)
        run([GO, 'test', '-tags=unit', '-run', '^$', './...', '-count=1'], backend,
            delivery / 'checks/backend-compile.log', TEST_ENV)

        context = work / 'image'
        context.mkdir()
        binary = context / 'sub2api'
        build_date = datetime.datetime.now(datetime.timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ')
        ldflags = ('-s -w -X main.Version=' + VERSION + ' -X main.Commit=' + commit +
                   ' -X main.Date=' + build_date + ' -X main.BuildType=release')
        run([GO, 'build', '-p', '2', '-tags', 'embed', '-trimpath', '-ldflags', ldflags,
             '-o', binary, './cmd/server'], backend, delivery / 'checks/backend-build.log', GO_ENV)
        shutil.copytree(source / 'backend/resources', context / 'resources')
        (context / 'Dockerfile').write_text(
            'FROM ' + BASE_IMAGE + '\nLABEL org.opencontainers.image.revision="' + commit +
            '"\nLABEL org.opencontainers.image.version="' + VERSION +
            '"\nCOPY --chown=1000:1000 sub2api /app/sub2api\n'
            'COPY --chown=1000:1000 resources /app/resources\n')
        run(DOCKER + ['build', '--platform', 'linux/amd64', '--pull=false', '--provenance=false',
                      '-t', image, context], REPO, delivery / 'checks/image-build.log')
        new_info = json.loads(run(DOCKER + ['image', 'inspect', image], REPO))[0]
        assert (new_info['Os'], new_info['Architecture']) == ('linux', 'amd64')
        assert commit in run(DOCKER + ['run', '--rm', '--pull', 'never', '--network', 'none',
                                       '--entrypoint', '/app/sub2api', image, '--version'], REPO)
        cid = run(DOCKER + ['run', '-d', '--pull', 'never', '--platform', 'linux/amd64',
                            '--network', 'none', '--no-healthcheck', image], REPO)
        try:
            for _ in range(20):
                probe = subprocess.run(DOCKER + ['exec', cid, 'wget', '-q', '-T', '2', '-O', '-',
                                                 'http://127.0.0.1:8080/'], env=ENV,
                                       capture_output=True, text=True)
                if probe.returncode == 0 and '<div id="app">' in probe.stdout:
                    (delivery / 'checks/frontend-smoke.txt').write_text('Embedded frontend served successfully.\n')
                    break
                time.sleep(1)
            else:
                raise RuntimeError('镜像内前端冒烟检查失败')
        finally:
            run(DOCKER + ['rm', '-f', '-v', cid], REPO)
        raw = work / 'image.tar'
        run(DOCKER + ['save', '-o', raw, image], REPO, delivery / 'checks/image-save.log')
        archive = delivery / ('sub2api-' + VERSION + '-' + commit[:9] + '.tar.gz')
        with raw.open('rb') as src, archive.open('wb') as dst:
            with gzip.GzipFile(filename='', mode='wb', fileobj=dst, compresslevel=6, mtime=0) as gz:
                shutil.copyfileobj(src, gz, 1024 * 1024)
        with tarfile.open(raw) as saved:
            saved_manifest = json.load(saved.extractfile('manifest.json'))
            config_digest = 'sha256:' + hashlib.sha256(saved.extractfile(saved_manifest[0]['Config']).read()).hexdigest()

        manifest = dict(target='ac.thinkerlu.top', release_branch='codex/release-sub2api',
                        upstream_tag=UPSTREAM_TAG, upstream_commit=UPSTREAM_COMMIT,
                        upstream_tree=UPSTREAM_TREE, commit=commit, version=VERSION, image=image,
                        image_id=new_info['Id'], image_config_digest=config_digest,
                        archive=archive.name, archive_sha256=sha(archive), archive_bytes=archive.stat().st_size,
                        rollback_commit=BASE_COMMIT, rollback_image=BASE_IMAGE,
                        rollback_image_id=BASE_ID, rollback_image_config_digest=BASE_CONFIG_ID,
                        rollback_binary_sha256=BASE_BINARY_SHA256,
                        binary_sha256=sha(binary), build_date=build_date,
                        database_migrations_changed=False, additive_migrations={},
                        delivery_revision='r1', image_backup_format='docker-save-gzip',
                        deployment_scripts_commit=commit,
                        source_package_target='ac.thinkerlu.top', compose_files=['docker-compose.yml'],
                        health_url='https://subapi.thinkerlu.top/health')
        for filename in ('deploy.sh', 'rollback.sh', 'release.py', 'README.md'):
            shutil.copy2(HERE / filename, delivery / filename)
        (delivery / 'manifest.json').write_text(json.dumps(manifest, indent=2) + '\n')
        with (delivery / 'source.patch').open('wb') as patch:
            subprocess.run(['git', 'diff', '--binary', BASE_COMMIT, commit], cwd=REPO, stdout=patch, check=True)
        shutil.copytree(REPO / 'docs/features', delivery / 'features')
        tests = subprocess.run(['python3', '-B', '-m', 'unittest', '-v', 'test_release.py'], cwd=HERE,
                               capture_output=True, text=True)
        (delivery / 'checks/release-tests.log').write_text(tests.stdout + tests.stderr)
        assert tests.returncode == 0, '主服务器脚本测试失败'
        lines = [sha(file) + '  ' + file.relative_to(delivery).as_posix() + '\n'
                 for file in sorted(delivery.rglob('*')) if file.is_file() and file.name != 'SHA256SUMS']
        (delivery / 'SHA256SUMS').write_text(''.join(lines))
        subprocess.run(['python3', '-B', '-c',
                        'import json,release; release.verify_package(json.loads((release.HERE / "manifest.json").read_text())); print("main package: PASS")'],
                       cwd=delivery, check=True)
        print('release=' + str(delivery))
        print('commit=' + commit)
        print('image=' + image)
        print('archive_sha256=' + manifest['archive_sha256'])


if __name__ == '__main__':
    main()
