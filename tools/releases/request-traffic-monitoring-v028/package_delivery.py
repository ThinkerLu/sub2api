#!/usr/bin/env python3
"""Assemble the uploadable delivery directory and SHA256SUMS."""
import hashlib
import json
from pathlib import Path
import shutil
import subprocess

HERE = Path(__file__).resolve().parent
REPO = Path('/Users/thinkerlu/Documents/apiProxy/sub2api/request-traffic-monitoring')
IMAGE = HERE / 'image'
DELIVERY = HERE / 'delivery'
DELIVERY.mkdir(exist_ok=True)

manifest = json.loads((IMAGE / 'manifest.json').read_text())
manifest.update(
    branch='codex/request-traffic-monitoring',
    rollback_commit=manifest['base_commit'],
    rollback_image='sub2api:v0.2.5-ws-turn-metadata-fallback-737912b14',
    scope='Merge v0.2.8 and add per-request body traffic metrics, session ID filtering, and dashboard traffic totals',
    database_migrations_changed=True,
    rollback_preserves_database=True,
    rollback_preserves_model_registry=True,
    remote_directory='/home/thinkerlu/sub2api-releases/request-traffic-monitoring-v028-' + manifest['commit'][:9],
)

for path in HERE.iterdir():
    if path.is_file() and path.name not in ('.gitignore', 'package_delivery.py'):
        shutil.copy2(path, DELIVERY / path.name)

for name in (
    manifest['archive'],
    manifest['archive'] + '.sha256',
    'manifest.json',
    'Dockerfile',
    'model-registry.example.yaml',
    'go-build-info.txt',
    'image-version.txt',
    'frontend-i18n.log',
    'frontend-typecheck.log',
    'frontend-build.log',
    'backend-build.log',
    'image-build.log',
    'linux-amd64-traffic-service.log',
    'linux-amd64-traffic-repository.log',
    'linux-amd64-traffic-middleware.log',
    'smoke-http.txt',
    'smoke-state.json',
):
    shutil.copy2(IMAGE / name, DELIVERY / name)

(DELIVERY / 'manifest.json').write_text(json.dumps(manifest, indent=2) + '\n')

with (DELIVERY / 'source.patch').open('wb') as output:
    subprocess.run(
        ['git', 'diff', '--binary', manifest['base_commit'], manifest['commit']],
        cwd=REPO,
        stdout=output,
        check=True,
    )

(DELIVERY / 'commits.txt').write_text(
    subprocess.check_output(
        ['git', 'log', '--oneline', manifest['base_commit'] + '..' + manifest['commit']],
        cwd=REPO,
        text=True,
    )
)

lines = []
for path in sorted(DELIVERY.iterdir()):
    if path.is_file() and path.name != 'SHA256SUMS':
        digest = hashlib.sha256()
        with path.open('rb') as source:
            for block in iter(lambda: source.read(1024 * 1024), b''):
                digest.update(block)
        lines.append(digest.hexdigest() + '  ' + path.name + '\n')
(DELIVERY / 'SHA256SUMS').write_text(''.join(lines))

print(manifest['remote_directory'])
print(len(lines) + 1, 'files')
