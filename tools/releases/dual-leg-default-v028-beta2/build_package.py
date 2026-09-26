#!/usr/bin/env python3
"""Build a release-branch-only Linux amd64 image and complete cc delivery."""

import datetime
import gzip
import hashlib
import json
import os
import re
from pathlib import Path
import shutil
import subprocess
import tarfile
import tempfile
import time
import sys


HERE = Path(__file__).resolve().parent
REPO = HERE.parents[2]
UPSTREAM_TAG = 'v0.2.8'
UPSTREAM_COMMIT = 'fd80b08c90b55edcad5b00171b53f08721d30da1'
VERSION = UPSTREAM_TAG + '-beta2-dual-leg-default'
BASE_RELEASE_COMMIT = '3fdfb9bb427eb6d23a17aa471c6c3c15e2821a2b'
BASE_RUNTIME_COMMIT = '3fdfb9bb427eb6d23a17aa471c6c3c15e2821a2b'
BASE_IMAGE = 'sub2api:v0.2.8-beta1-dual-leg-thinking-ws-3fdfb9bb4'
BASE_ID = 'sha256:a0d31f2a6965dcda5f62f36986a0fa84a9129fc961bbc27b7e61a03dd62a7ac8'
BASE_CONFIG_ID = 'sha256:4efb957933bee20bb7f7010eac095a0ca630bd30e6a13d2cf123fd3395e38f9e'
BASE_BINARY_SHA256 = '1634c520514c2cc9aa4afe685dc4ddccd1b7725f085043fd16ab8cb5031dfaa6'
MIGRATION = '242_usage_log_ingress_transport.sql'
GO = os.environ.get('SUB2API_GO', '/private/tmp/go1.27.0/bin/go')
NODE = '/Users/thinkerlu/.cache/codex-runtimes/codex-primary-runtime/dependencies/node/bin'
DOCKER = ['docker', '--context', 'colima-sub2api-build']
ENV = {**os.environ, 'PATH': NODE + ':/opt/homebrew/bin:' + os.environ.get('PATH', '')}
GO_ENV = {**ENV, 'CGO_ENABLED': '0', 'GOOS': 'linux', 'GOARCH': 'amd64',
          'GOTOOLCHAIN': 'local', 'GOCACHE': '/private/tmp/sub2api-traffic-go-cache'}
TEST_ENV = {key: value for key, value in ENV.items() if key not in ('GOOS', 'GOARCH', 'CGO_ENABLED')}
TEST_ENV.update(GOTOOLCHAIN='local', GOCACHE='/private/tmp/sub2api-traffic-go-cache')


def run(args, cwd, log=None, env=ENV):
    result = subprocess.run([str(arg) for arg in args], cwd=cwd, env=env, capture_output=True, text=True)
    if log:
        log.write_text(result.stdout + result.stderr)
    if result.returncode:
        raise RuntimeError('构建命令失败: ' + ' '.join(str(arg) for arg in args[:3]) +
                           ('；见 ' + str(log) if log else ''))
    return result.stdout.strip()


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
    assert run(['git', 'rev-parse', UPSTREAM_TAG + '^{commit}'], REPO) == UPSTREAM_COMMIT, '官方基线 tag 不匹配'
    run(['git', 'merge-base', '--is-ancestor', UPSTREAM_COMMIT, commit], REPO)
    run(['git', 'merge-base', '--is-ancestor', BASE_RELEASE_COMMIT, commit], REPO)
    run(['git', 'diff', '--exit-code', BASE_RELEASE_COMMIT, commit, '--', 'Dockerfile', 'deploy/docker-entrypoint.sh'], REPO)
    changed_migrations = run(['git', 'diff', '--name-only', BASE_RELEASE_COMMIT, commit, '--', 'backend/migrations/*.sql'], REPO).splitlines()
    assert changed_migrations == [], 'beta2 不允许新增或改写迁移'
    info = json.loads(run(DOCKER + ['image', 'inspect', BASE_IMAGE], REPO))[0]
    assert info['Id'] == BASE_ID and (info['Os'], info['Architecture']) == ('linux', 'amd64'), '旧镜像摘要或平台不符'

    version = VERSION
    image = 'sub2api:' + version + '-' + commit[:9]
    out = HERE / 'image'
    delivery = HERE / 'delivery'
    assert not out.exists() and not delivery.exists(), '已有构建产物，请人工检查后选择新目录'
    out.mkdir()
    delivery.mkdir()
    with tempfile.TemporaryDirectory(prefix='sub2api-v028-beta2-build-', dir='/private/tmp') as tmp:
        work = Path(tmp)
        source_tar = work / 'source.tar'
        run(['git', 'archive', '--format=tar', '-o', source_tar, commit], REPO)
        source = work / 'source'
        source.mkdir()
        with tarfile.open(source_tar) as archive:
            archive.extractall(source, filter='data')
        run([sys.executable, '-m', 'unittest', '-v', 'test_release.py'],
            source / HERE.relative_to(REPO), out / 'release-tests.log')
        dist = source / 'backend/internal/web/dist'
        if dist.exists():
            shutil.rmtree(dist)
        dependencies = REPO / 'frontend/node_modules'
        if not dependencies.is_dir():
            dependencies = REPO.parent / 'request-traffic-monitoring/frontend/node_modules'
        assert dependencies.is_dir(), '缺少已安装前端依赖'
        shutil.copytree(dependencies, source / 'frontend/node_modules', symlinks=True)
        frontend = source / 'frontend'
        print('验证前端和构建嵌入资源', flush=True)
        run(['./node_modules/.bin/vitest', 'run', 'src/i18n/__tests__/localeKeyCompleteness.spec.ts',
             'src/utils/__tests__/adaptiveWs.spec.ts', 'src/components/account/__tests__/CreateAccountModal.spec.ts',
             'src/components/account/__tests__/EditAccountModal.spec.ts',
             'src/components/admin/usage/__tests__/UsageTable.spec.ts', 'src/components/admin/usage/__tests__/UsageFilters.spec.ts',
             'src/views/admin/__tests__/UsageView.spec.ts', 'src/views/user/__tests__/UsageView.spec.ts'], frontend, out / 'frontend-tests.log')
        run(['./node_modules/.bin/vue-tsc', '--noEmit'], frontend, out / 'frontend-typecheck.log')
        run(['./node_modules/.bin/vite', 'build'], frontend, out / 'frontend-build.log')
        assert (dist / 'index.html').is_file(), '前端内嵌资源未生成'

        context = work / 'image'
        context.mkdir()
        binary = context / 'sub2api'
        build_date = datetime.datetime.now(datetime.timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ')
        ldflags = ('-s -w -X main.Version=' + version + ' -X main.Commit=' + commit +
                   ' -X main.Date=' + build_date + ' -X main.BuildType=release')
        print('验证后端、迁移、真实 Codex 客户端和 PostgreSQL', flush=True)
        run([GO, 'test', './internal/service', './internal/service/openai_ws_v2', './internal/repository',
             './internal/handler', './internal/handler/admin', './internal/handler/dto', './internal/config', './migrations', '-count=1'],
            source / 'backend', out / 'backend-tests.log', TEST_ENV)
        codex = os.environ.get('SUB2API_CODEX_PROBE_BIN', '/Applications/ChatGPT.app/Contents/Resources/codex')
        codex_version = run([codex, '--version'], source)
        run([GO, 'test', './internal/service', '-run', '^TestCodexWS', '-count=1', '-v'],
            source / 'backend', out / 'codex-probe.log', {**TEST_ENV, 'SUB2API_CODEX_PROBE_BIN': codex})
        run([GO, 'test', './internal/repository', '-run', '^TestUsageIngressTransportPostgresRoundTrip$', '-count=1', '-v'],
            source / 'backend', out / 'postgres-probe.log', {**TEST_ENV, 'SUB2API_POSTGRES_PROBE_CONTEXT': 'colima-sub2api-build'})
        run([GO, 'test', '-race', './internal/service', './internal/repository', './internal/handler',
             '-run', '^TestWSRecovery|^TestWSMigration|^TestWSModelMigrationHandler|^TestDualLeg|^TestWSAdaptiveSocket|^TestWSRetained', '-count=1'],
            source / 'backend', out / 'race-tests.log', TEST_ENV)
        print('验证 Linux/amd64 大帧内存准入', flush=True)
        probe = work / 'service.test'
        run([GO, 'test', '-c', '-p', '2', '-o', probe, './internal/service'], source / 'backend', out / 'memory-probe-build.log', GO_ENV)
        probe_cid = run(DOCKER + ['create', '--platform', 'linux/amd64', '--memory=1g', '--network=none', '--no-healthcheck',
                                 '-e', 'GOMEMLIMIT=768MiB', '-e', 'SUB2API_WS_STRESS=1', '--entrypoint', '/tmp/dual-leg-test',
                                 BASE_IMAGE, '-test.run', '^TestDualLegLargeFrameWithoutBudget$', '-test.v'], REPO)
        try:
            run(DOCKER + ['cp', probe, probe_cid + ':/tmp/dual-leg-test'], REPO)
            memory_output = run(DOCKER + ['start', '-a', probe_cid], REPO, out / 'linux-memory-probe.log')
            state = json.loads(run(DOCKER + ['inspect', probe_cid], REPO))[0]['State']
            assert state['ExitCode'] == 0 and not state['OOMKilled'], 'Linux 内存验证失败'
            peak = re.search(r'VmHWM:\s+(\d+)\s+kB', memory_output)
            assert peak, '缺少峰值 RSS 记录'
            peak_rss_kib = int(peak.group(1))
        finally:
            run(DOCKER + ['rm', '-f', '-v', probe_cid], REPO)
        print('构建发布二进制与镜像', flush=True)
        run([GO, 'build', '-p', '2', '-tags', 'embed', '-trimpath', '-ldflags', ldflags,
             '-o', binary, './cmd/server'], source / 'backend', out / 'backend-build.log', GO_ENV)
        shutil.copytree(source / 'backend/resources', context / 'resources')
        (context / 'Dockerfile').write_text(
            'FROM ' + BASE_IMAGE + '\nLABEL org.opencontainers.image.revision="' + commit +
            '"\nLABEL org.opencontainers.image.version="' + version +
            '"\nCOPY --chown=1000:1000 sub2api /app/sub2api\n'
            'COPY --chown=1000:1000 resources /app/resources\n')
        run(DOCKER + ['build', '--platform', 'linux/amd64', '--pull=false', '--provenance=false',
                      '-t', image, context], REPO, out / 'image-build.log')
        new_info = json.loads(run(DOCKER + ['image', 'inspect', image], REPO))[0]
        assert (new_info['Os'], new_info['Architecture']) == ('linux', 'amd64')
        actual_version = run(DOCKER + ['run', '--rm', '--pull', 'never', '--network', 'none',
                                       '--entrypoint', '/app/sub2api', image, '--version'], REPO)
        assert commit in actual_version and version in actual_version, '镜像内版本或提交不匹配'
        cid = run(DOCKER + ['run', '-d', '--pull', 'never', '--platform', 'linux/amd64',
                            '--network', 'none', '--no-healthcheck', image], REPO)
        try:
            for _ in range(20):
                probe = subprocess.run(DOCKER + ['exec', cid, 'wget', '-q', '-T', '2', '-O', '-',
                                                 'http://127.0.0.1:8080/'], env=ENV,
                                       capture_output=True, text=True)
                if probe.returncode == 0 and '<div id="app">' in probe.stdout:
                    (out / 'frontend-smoke.txt').write_text('Embedded frontend served successfully.\n')
                    break
                time.sleep(1)
            else:
                raise RuntimeError('镜像内前端冒烟检查失败')
        finally:
            run(DOCKER + ['rm', '-f', '-v', cid], REPO)
        raw = work / 'image.tar'
        run(DOCKER + ['save', '-o', raw, image], REPO, out / 'image-save.log')
        archive = delivery / ('sub2api-' + version + '-' + commit[:9] + '.tar.gz')
        with raw.open('rb') as src, archive.open('wb') as dst:
            with gzip.GzipFile(filename='', mode='wb', fileobj=dst, compresslevel=6, mtime=0) as gz:
                shutil.copyfileobj(src, gz, 1024 * 1024)
        with tarfile.open(raw) as saved:
            saved_manifest = json.load(saved.extractfile('manifest.json'))
            config_digest = 'sha256:' + hashlib.sha256(saved.extractfile(saved_manifest[0]['Config']).read()).hexdigest()

        manifest = dict(target='cc.thkl.site', release_branch='codex/release-sub2api',
                        upstream_tag=UPSTREAM_TAG, upstream_commit=UPSTREAM_COMMIT,
                        commit=commit, version=version, image=image,
                        image_id=new_info['Id'], image_config_digest=config_digest,
                        archive=archive.name, archive_sha256=sha(archive),
                        rollback_commit=BASE_RUNTIME_COMMIT, rollback_image=BASE_IMAGE,
                        rollback_image_id=BASE_ID, rollback_image_config_digest=BASE_CONFIG_ID,
                        rollback_binary_sha256=BASE_BINARY_SHA256,
                        archive_bytes=archive.stat().st_size,
                        binary_sha256=sha(binary), build_date=build_date,
                        database_migrations_changed=False,
                        additive_migrations={},
                        codex_probe_version=codex_version, memory_probe_peak_rss_kib=peak_rss_kib,
                        memory_probe_container_limit_bytes=1024*1024*1024, memory_budget_removed=True,
                        memory_probe_host_arch=run(DOCKER + ['info', '--format', '{{.Architecture}}'], REPO),
                        dual_leg_default_enabled=True, custom_feature_env_gates_removed=True)
        for filename in ('deploy.sh', 'rollback.sh', 'release.py', 'README.md'):
            shutil.copy2(HERE / filename, delivery / filename)
        shutil.copy2(source / 'backend/migrations' / MIGRATION, delivery / MIGRATION)
        shutil.copytree(source / 'docs/features', delivery / 'features')
        checks = delivery / 'checks'
        checks.mkdir()
        for filename in ('release-tests.log', 'frontend-tests.log', 'frontend-typecheck.log', 'backend-tests.log', 'codex-probe.log', 'postgres-probe.log', 'race-tests.log', 'linux-memory-probe.log'):
            shutil.copy2(out / filename, checks / filename)
        (delivery / 'manifest.json').write_text(json.dumps(manifest, indent=2) + '\n')
        with (delivery / 'source.patch').open('wb') as patch:
            subprocess.run(['git', 'diff', '--binary', BASE_RUNTIME_COMMIT, commit],
                           cwd=REPO, stdout=patch, check=True)
        lines = [sha(file) + '  ' + file.relative_to(delivery).as_posix() + '\n'
                 for file in sorted(delivery.rglob('*')) if file.is_file() and file.name != 'SHA256SUMS']
        (delivery / 'SHA256SUMS').write_text(''.join(lines))
        run([sys.executable, '-c', 'import json, release; release.verify_package(json.loads((release.HERE / "manifest.json").read_text()))'], delivery, out / 'package-verification.log')
        print('release=' + str(delivery))
        print('commit=' + commit)
        print('image=' + image)
        print('archive_sha256=' + manifest['archive_sha256'])


if __name__ == '__main__':
    main()
