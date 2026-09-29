#!/usr/bin/env python3
"""Assemble the beta1 r2 delivery with optional local-image rollback."""

import hashlib
import json
from pathlib import Path
import shutil
import subprocess
import sys


HERE = Path(__file__).resolve().parent
REPO = HERE.parents[2]
APP_COMMIT = '79a5250cdfb8ca8c056b9e499fa9d762b27ec3d8'


def git(*args):
    return subprocess.check_output(['git', *args], cwd=REPO, text=True).strip()


def sha(path):
    digest = hashlib.sha256()
    with Path(path).open('rb') as source:
        for block in iter(lambda: source.read(1024 * 1024), b''):
            digest.update(block)
    return digest.hexdigest()


def verify_files(root):
    checked = set()
    for line in (root / 'SHA256SUMS').read_text().splitlines():
        expected, name = line.split('  ', 1)
        path = root / name
        assert not Path(name).is_absolute() and '..' not in Path(name).parts and not path.is_symlink()
        assert name not in checked and path.is_file() and sha(path) == expected
        checked.add(name)
    assert {path.relative_to(root).as_posix() for path in root.rglob('*') if path.is_file()} == checked | {'SHA256SUMS'}


def main():
    assert git('symbolic-ref', '--short', 'HEAD') == 'codex/release-sub2api', '只允许发布分支组包'
    assert not git('status', '--porcelain'), '发布 worktree 必须干净'
    scripts_commit = git('rev-parse', 'HEAD')
    source = HERE / 'delivery-r1'
    delivery = HERE / 'delivery-r2'
    assert source.is_dir() and not delivery.exists(), '缺少 r1 原包或 r2 包已存在'
    manifest = json.loads((source / 'manifest.json').read_text())
    assert manifest['commit'] == APP_COMMIT and manifest['delivery_revision'] == 'r1'
    verify_files(source)
    subprocess.run([sys.executable, '-B', '-c',
                    'import json,release; release.verify_package(json.loads((release.HERE / "manifest.json").read_text())); print("r1 package: PASS")'],
                   cwd=source, check=True)

    shutil.copytree(source, delivery)
    for name in ('release.py', 'deploy.sh', 'rollback.sh', 'README.md'):
        shutil.copy2(HERE / name, delivery / name)
    shutil.copytree(REPO / 'docs/features', delivery / 'features', dirs_exist_ok=True)
    manifest.update(
        delivery_revision='r2',
        image_backup_format='docker-save-gzip-or-local-tag',
        deployment_scripts_commit=scripts_commit,
        base_package_sha256=sha(source / 'SHA256SUMS'),
        base_package_manifest_sha256=sha(source / 'manifest.json'),
    )
    (delivery / 'manifest.json').write_text(json.dumps(manifest, indent=2) + '\n')
    tests = subprocess.run([sys.executable, '-B', '-m', 'unittest', '-v', 'test_release.py'], cwd=HERE,
                           capture_output=True, text=True)
    (delivery / 'checks/release-tests.log').write_text(tests.stdout + tests.stderr)
    assert tests.returncode == 0, 'r2 发布脚本测试失败'
    (delivery / 'SHA256SUMS').write_text(''.join(
        sha(path) + '  ' + path.relative_to(delivery).as_posix() + '\n'
        for path in sorted(delivery.rglob('*')) if path.is_file() and path.name != 'SHA256SUMS'
    ))
    verify_files(delivery)
    subprocess.run([sys.executable, '-B', '-c',
                    'import json,release; release.verify_package(json.loads((release.HERE / "manifest.json").read_text())); print("r2 package: PASS")'],
                   cwd=delivery, check=True)
    print(delivery)


if __name__ == '__main__':
    main()
