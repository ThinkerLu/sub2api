#!/usr/bin/env python3
"""Retarget the verified beta3 image with main-server-only deployment tooling."""
import hashlib
import json
from pathlib import Path
import shutil
import subprocess
import sys

import release

HERE = Path(__file__).resolve().parent
REPO = HERE.parents[2]
UPSTREAM_TAG = 'v0.2.8'
VERSION = 'v0.2.8-beta3-adaptive-ws-keepalive'
IMAGE_COMMIT = 'b3a100e05054b265a2e0a8a71827c659553503e9'


def git(*args):
    return subprocess.check_output(['git', *args], cwd=REPO, text=True).strip()


def main():
    assert git('symbolic-ref', '--short', 'HEAD') == 'codex/release-sub2api'
    assert not git('status', '--porcelain'), '发布 worktree 必须干净'
    scripts_commit = git('rev-parse', 'HEAD')
    source = REPO / 'tools/releases/adaptive-ws-keepalive-v028-beta3/delivery'
    manifest = json.loads((source / 'manifest.json').read_text())
    assert manifest['commit'] == IMAGE_COMMIT and manifest['version'] == VERSION
    assert manifest['target'] == 'cc.thkl.site'
    checked = set()
    for line in (source / 'SHA256SUMS').read_text().splitlines():
        expected, name = line.split('  ', 1)
        path = source / name
        assert not Path(name).is_absolute() and '..' not in Path(name).parts and not path.is_symlink()
        assert name not in checked and release.digest(path) == expected
        checked.add(name)
    assert {p.relative_to(source).as_posix() for p in source.rglob('*') if p.is_file()} == checked | {'SHA256SUMS'}
    assert release.digest(source / manifest['archive']) == manifest['archive_sha256']
    changed = git('diff', '--name-only', release.BASE_COMMIT, IMAGE_COMMIT, '--', 'backend/migrations/*.sql').splitlines()
    assert changed == ['backend/migrations/' + release.ADDITIVE_MIGRATION]
    delivery = HERE / 'delivery'
    assert not delivery.exists(), '已有主机包，请勿覆盖'
    shutil.copytree(source, delivery)
    for name in ('deploy.sh', 'rollback.sh', 'release.py', 'README.md'):
        shutil.copy2(HERE / name, delivery / name)
    for name in ('README.md', '2026-09-26-main-beta3-delivery.md'):
        shutil.copy2(REPO / 'docs/features' / name, delivery / 'features' / name)
    manifest.update(target='ac.thinkerlu.top', deployment_scripts_commit=scripts_commit,
                    source_package_target='cc.thkl.site', compose_files=['docker-compose.yml'],
                    health_url='https://subapi.thinkerlu.top/health', rollback_commit=release.BASE_COMMIT,
                    rollback_image=release.BASE_IMAGE, rollback_image_id=release.BASE_ID,
                    rollback_image_config_digest=release.BASE_CONFIG_ID,
                    rollback_binary_sha256=release.BASE_BINARY_SHA256,
                    database_migrations_changed=True,
                    additive_migrations={release.ADDITIVE_MIGRATION: hashlib.sha256((delivery / release.ADDITIVE_MIGRATION).read_text().strip().encode()).hexdigest()})
    (delivery / 'manifest.json').write_text(json.dumps(manifest, indent=2) + '\n')
    (delivery / 'source.patch').write_bytes(subprocess.check_output(['git', 'diff', '--binary', release.BASE_COMMIT, IMAGE_COMMIT], cwd=REPO))
    (delivery / 'deployment-scripts.patch').write_bytes(subprocess.check_output(
        ['git', 'diff', '--binary', IMAGE_COMMIT, scripts_commit, '--', str(HERE.relative_to(REPO)), 'docs/features'], cwd=REPO))
    tests = subprocess.run([sys.executable, '-B', '-m', 'unittest', '-v', 'test_release.py'], cwd=HERE, capture_output=True, text=True)
    (delivery / 'checks/main-release-tests.log').write_text(tests.stdout + tests.stderr)
    assert tests.returncode == 0, '主服务器脚本测试失败'
    (delivery / 'SHA256SUMS').write_text(''.join(release.digest(p) + '  ' + p.relative_to(delivery).as_posix() + '\n'
        for p in sorted(delivery.rglob('*')) if p.is_file() and p.name != 'SHA256SUMS'))
    subprocess.run([sys.executable, '-B', '-c', 'import json,release; release.verify_package(json.loads((release.HERE / "manifest.json").read_text())); print("main package: PASS")'], cwd=delivery, check=True)
    print(delivery)


if __name__ == '__main__':
    main()
