#!/usr/bin/env python3
"""Build a release-branch-only Linux amd64 image and complete cc delivery."""

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
BASE_RELEASE_COMMIT = '449a8c8b03e1d9103198b3fb589bf2b6cf752b55'
BASE_RUNTIME_COMMIT = '7a3472327b70c196089fc07c44ec5d08a3debb62'
BASE_IMAGE = 'sub2api:v0.2.8-request-traffic-monitoring-7a3472327'
BASE_ID = 'sha256:f3a7f2f72199af2fe0a6921163e207dc3d8848c6575344576a4e28bd45931bba'
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
    run(['git', 'merge-base', '--is-ancestor', BASE_RELEASE_COMMIT, commit], REPO)
    run(['git', 'diff', '--exit-code', BASE_RELEASE_COMMIT, commit, '--', 'Dockerfile', 'deploy/docker-entrypoint.sh'], REPO)
    info = json.loads(run(DOCKER + ['image', 'inspect', BASE_IMAGE], REPO))[0]
    assert info['Id'] == BASE_ID and (info['Os'], info['Architecture']) == ('linux', 'amd64'), '旧镜像摘要或平台不符'

    version = 'v0.2.9-traffic-trend-dashboard'
    image = 'sub2api:' + version + '-' + commit[:9]
    out = HERE / 'image'
    delivery = HERE / 'delivery'
    assert not out.exists() and not delivery.exists(), '已有构建产物，请人工检查后选择新目录'
    out.mkdir()
    delivery.mkdir()
    with tempfile.TemporaryDirectory(prefix='sub2api-v029-build-', dir='/private/tmp') as tmp:
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
        run(['./node_modules/.bin/vitest', 'run', 'src/i18n/__tests__/localeKeyCompleteness.spec.ts',
             'src/views/admin/__tests__/DashboardView.spec.ts',
             'src/components/charts/__tests__/TrafficUsageTrend.spec.ts'], frontend, out / 'frontend-tests.log')
        run(['./node_modules/.bin/vue-tsc', '--noEmit'], frontend, out / 'frontend-typecheck.log')
        run(['./node_modules/.bin/vite', 'build'], frontend, out / 'frontend-build.log')
        assert (dist / 'index.html').is_file(), '前端内嵌资源未生成'

        context = work / 'image'
        context.mkdir()
        binary = context / 'sub2api'
        build_date = datetime.datetime.now(datetime.timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ')
        ldflags = ('-s -w -X main.Version=' + version + ' -X main.Commit=' + commit +
                   ' -X main.Date=' + build_date + ' -X main.BuildType=release')
        run([GO, 'test', './internal/repository', '-run',
             '^(TestUsageTrendIncludesTrafficForEveryQueryPath|TestUserUsageTrendIncludesTraffic)$', '-count=1'],
            source / 'backend', out / 'backend-tests.log', TEST_ENV)
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
                        commit=commit, version=version, image=image,
                        image_id=new_info['Id'], image_config_digest=config_digest,
                        archive=archive.name, archive_sha256=sha(archive),
                        rollback_commit=BASE_RUNTIME_COMMIT, rollback_image=BASE_IMAGE,
                        rollback_image_id=BASE_ID, archive_bytes=archive.stat().st_size,
                        binary_sha256=sha(binary), build_date=build_date,
                        database_migrations_changed=False)
        for filename in ('deploy.sh', 'rollback.sh', 'release.py', 'README.md'):
            shutil.copy2(HERE / filename, delivery / filename)
        (delivery / 'manifest.json').write_text(json.dumps(manifest, indent=2) + '\n')
        with (delivery / 'source.patch').open('wb') as patch:
            subprocess.run(['git', 'diff', '--binary', BASE_RUNTIME_COMMIT, commit],
                           cwd=REPO, stdout=patch, check=True)
        lines = [sha(file) + '  ' + file.name + '\n'
                 for file in sorted(delivery.iterdir()) if file.is_file() and file.name != 'SHA256SUMS']
        (delivery / 'SHA256SUMS').write_text(''.join(lines))
        print('release=' + str(delivery))
        print('commit=' + commit)
        print('image=' + image)
        print('archive_sha256=' + manifest['archive_sha256'])


if __name__ == '__main__':
    main()
