#!/usr/bin/env python3
"""Build the committed frontend/backend and Linux amd64 traffic-monitoring image."""
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
REPO = Path('/Users/thinkerlu/Documents/apiProxy/sub2api/request-traffic-monitoring')
COMMIT = os.environ['SUB2API_RELEASE_COMMIT']
BASE_COMMIT = '737912b14364834ef666599c1473491b814e416e'
VERSION = os.environ.get('SUB2API_RELEASE_VERSION', 'v0.2.8-request-traffic-monitoring')
IMAGE_TAG = os.environ.get('SUB2API_RELEASE_IMAGE_TAG', 'v0.2.8-request-traffic-monitoring')
IMAGE = 'sub2api:' + IMAGE_TAG + '-' + COMMIT[:9]
BASE_IMAGE = 'sub2api:v0.2.5-ws-turn-metadata-fallback-737912b14'
BASE_IMAGE_ID = 'sha256:268a4f3009d5ded0efd0aabc1efa0a0a5d3d92f43c089ab13d352dcae09fded5'
GO = os.environ.get('SUB2API_GO', '/private/tmp/go1.27.0/bin/go')
NODE_BIN = Path('/Users/thinkerlu/.cache/codex-runtimes/codex-primary-runtime/dependencies/node/bin')
DOCKER = ['/opt/homebrew/bin/docker', '--context', 'colima-sub2api-build']
OUT = Path(os.environ.get('SUB2API_RELEASE_OUTPUT', str(HERE / 'image')))
OUT.mkdir(exist_ok=True)
WORK = Path(tempfile.mkdtemp(prefix='sub2api-v028-traffic-build-', dir='/private/tmp'))
ENV = {**os.environ, 'PATH': str(NODE_BIN) + ':/opt/homebrew/bin:' + os.environ.get('PATH', '')}
GO_ENV = {**ENV, 'CGO_ENABLED': '0', 'GOOS': 'linux', 'GOARCH': 'amd64', 'GOTOOLCHAIN': 'local'}


def sha(path):
    with Path(path).open('rb') as f:
        digest = hashlib.sha256()
        for block in iter(lambda: f.read(1024 * 1024), b''):
            digest.update(block)
        return digest.hexdigest()


def sha_trimmed(path):
    return hashlib.sha256(Path(path).read_text().strip().encode()).hexdigest()


def run(args, log, cwd=WORK, env=ENV):
    print('RUN', ' '.join(map(str, args)), flush=True)
    result = subprocess.run(list(map(str, args)), cwd=cwd, env=env, text=True,
                            stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    (OUT / log).write_text(result.stdout)
    if result.returncode:
        print(result.stdout[-5000:], flush=True)
        raise RuntimeError(f'Command failed ({result.returncode}); see {OUT / log}')
    return result.stdout


def run_linux_test(image, test_binary, name, pattern, log_name):
    cid = run(DOCKER + ['create', '--pull', 'never', '--platform', 'linux/amd64',
                        '--network', 'none', '--entrypoint', '/tmp/' + name, image,
                        '-test.run', pattern, '-test.v'], log_name + '-create.log').strip()
    try:
        run(DOCKER + ['cp', test_binary, f'{cid}:/tmp/{name}'], log_name + '-copy.log')
        run(DOCKER + ['start', '-a', cid], log_name)
        state = json.loads(run(DOCKER + ['inspect', cid], log_name + '-inspect.json'))[0]['State']
        assert state['ExitCode'] == 0 and not state['OOMKilled'], state
    finally:
        run(DOCKER + ['rm', '-f', '-v', cid], log_name + '-cleanup.log')


run(['git', 'merge-base', '--is-ancestor', BASE_COMMIT, COMMIT], 'ancestry-baseline.log', REPO)
run(['git', 'diff', '--exit-code', BASE_COMMIT, COMMIT, '--', 'Dockerfile', 'deploy/docker-entrypoint.sh'], 'runtime-compatibility.log', REPO)
base = json.loads(run(DOCKER + ['image', 'inspect', BASE_IMAGE], 'base-image.json'))[0]
assert base['Id'] == BASE_IMAGE_ID and base['Architecture'] == 'amd64' and base['Os'] == 'linux'
run(['git', 'archive', '--format=tar', '-o', WORK / 'source.tar', COMMIT], 'source-export.log', REPO)
source = WORK / 'source'
source.mkdir()
with tarfile.open(WORK / 'source.tar') as f:
    f.extractall(source)
dist = source / 'backend/internal/web/dist'
if dist.exists():
    shutil.rmtree(dist)
shutil.copytree(REPO / 'frontend/node_modules', source / 'frontend/node_modules', symlinks=True)
frontend = source / 'frontend'
run(['./node_modules/.bin/vitest', 'run', 'src/i18n/__tests__/localeKeyCompleteness.spec.ts'], 'frontend-i18n.log', frontend)
run(['./node_modules/.bin/vue-tsc', '--noEmit'], 'frontend-typecheck.log', frontend)
run(['./node_modules/.bin/vite', 'build'], 'frontend-build.log', frontend)
assert (dist / 'index.html').is_file(), 'Frontend build output missing'

build_date = os.environ.get('SUB2API_BUILD_DATE') or datetime.datetime.now(datetime.timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ')
context = WORK / 'image'
context.mkdir()
binary = context / 'sub2api'
ldflags = f'-s -w -X main.Version={VERSION} -X main.Commit={COMMIT} -X main.Date={build_date} -X main.BuildType=release'
run([GO, 'build', '-p', '2', '-tags', 'embed', '-trimpath', '-ldflags', ldflags, '-o', binary, './cmd/server'],
    'backend-build.log', source / 'backend', GO_ENV)
run([GO, 'version', '-m', binary], 'go-build-info.txt')
binary_sha = sha(binary)
shutil.copytree(source / 'backend/resources', context / 'resources')
shutil.copy2(REPO / 'deploy/model-registry.example.yaml', OUT / 'model-registry.example.yaml')
dockerfile = f'FROM {BASE_IMAGE}\nLABEL org.opencontainers.image.revision="{COMMIT}"\nLABEL org.opencontainers.image.version="{VERSION}"\nCOPY --chown=1000:1000 sub2api /app/sub2api\nCOPY --chown=1000:1000 resources /app/resources\n'
(context / 'Dockerfile').write_text(dockerfile)
(OUT / 'Dockerfile').write_text(dockerfile)
run(DOCKER + ['build', '--platform', 'linux/amd64', '--pull=false', '--provenance=false', '-t', IMAGE, context], 'image-build.log')
inspection = json.loads(run(DOCKER + ['image', 'inspect', IMAGE], 'image-inspect.json'))[0]
assert inspection['Architecture'] == 'amd64' and inspection['Os'] == 'linux'
version = run(DOCKER + ['run', '--rm', '--pull', 'never', '--platform', 'linux/amd64', '--network', 'none',
                        '--entrypoint', '/app/sub2api', IMAGE, '--version'], 'image-version.txt')
assert COMMIT in version and VERSION in version

checks = WORK / 'checks'
checks.mkdir()
for package, name, pattern, log_name in (
    ('./internal/service', 'traffic-service.test',
     'TestUsageTrafficCounterSnapshotAndContextResolution|TestUsageTrafficReadCloserCountsReadBytes', 'traffic-service.log'),
    ('./internal/repository', 'traffic-repository.test',
     'TestHTTPUpstreamUsageTrafficTracking|TestUsageLogRepositoryListWithFiltersSessionID', 'traffic-repository.log'),
    ('./internal/server/middleware', 'traffic-middleware.test',
     'TestUsageTrafficMiddlewareCountsRequestAndResponseBodies', 'traffic-middleware.log'),
):
    test_binary = checks / name
    run([GO, 'test', '-c', '-o', test_binary, package], 'linux-' + name + '-build.log', source / 'backend', GO_ENV)
    run_linux_test(IMAGE, test_binary, name, pattern, 'linux-amd64-' + log_name)

cid = run(DOCKER + ['run', '-d', '--pull', 'never', '--platform', 'linux/amd64', '--network', 'none', '--no-healthcheck', IMAGE], 'smoke-container.log').strip()
try:
    for _ in range(20):
        r = subprocess.run(DOCKER + ['exec', cid, 'wget', '-q', '-T', '2', '-O', '-', 'http://127.0.0.1:8080/'], env=ENV, capture_output=True, text=True)
        if r.returncode == 0 and '<div id="app">' in r.stdout:
            (OUT / 'smoke-http.txt').write_text('GET / returned the embedded frontend successfully (setup mode, no database).\n')
            break
        time.sleep(1)
    else:
        raise RuntimeError('Embedded frontend smoke check failed')
finally:
    run(DOCKER + ['logs', cid], 'smoke.log')
    run(DOCKER + ['stop', '-t', '10', cid], 'smoke-stop.log')
    state = json.loads(run(DOCKER + ['inspect', cid], 'smoke-inspect.json'))[0]['State']
    (OUT / 'smoke-state.json').write_text(json.dumps(state, indent=2))
    run(DOCKER + ['rm', '-v', cid], 'smoke-cleanup.log')
    assert not state['OOMKilled']

raw = WORK / 'image.tar'
run(DOCKER + ['save', '-o', raw, IMAGE], 'image-save.log')
archive = OUT / f'sub2api-{IMAGE_TAG}-{COMMIT[:9]}.tar.gz'
with raw.open('rb') as src, archive.open('wb') as dst, gzip.GzipFile(filename='', mode='wb', fileobj=dst, compresslevel=6, mtime=0) as gz:
    shutil.copyfileobj(src, gz, 1024 * 1024)
with gzip.open(archive, 'rb') as f:
    while f.read(1024 * 1024):
        pass
with tarfile.open(raw) as image_tar:
    docker_manifest = json.load(image_tar.extractfile('manifest.json'))
    config_bytes = image_tar.extractfile(docker_manifest[0]['Config']).read()
    config_digest = 'sha256:' + hashlib.sha256(config_bytes).hexdigest()
archive_sha = sha(archive)
(OUT / (archive.name + '.sha256')).write_text(archive_sha + '  ' + archive.name + '\n')
manifest = dict(
    commit=COMMIT,
    base_commit=BASE_COMMIT,
    version=VERSION,
    image=IMAGE,
    image_id=inspection['Id'],
    image_config_digest=config_digest,
    platform='linux/amd64',
    runtime_base=BASE_IMAGE,
    runtime_base_id=BASE_IMAGE_ID,
    build_date=build_date,
    binary_sha256=binary_sha,
    archive=archive.name,
    archive_sha256=archive_sha,
    archive_bytes=archive.stat().st_size,
    upstream_tag='v0.2.8',
    migration_238b_content_moderation_engine_meta_sha256=sha_trimmed(source / 'backend/migrations/238b_content_moderation_engine_meta.sql'),
    migration_239_channel_reasoning_effort_multipliers_sha256=sha_trimmed(source / 'backend/migrations/239_channel_reasoning_effort_multipliers.sql'),
    migration_240_affiliate_ledger_operation_id_sha256=sha_trimmed(source / 'backend/migrations/240_affiliate_ledger_operation_id.sql'),
    migration_241_usage_log_traffic_bytes_sha256=sha_trimmed(source / 'backend/migrations/241_usage_log_traffic_bytes.sql'),
)
(OUT / 'manifest.json').write_text(json.dumps(manifest, indent=2) + '\n')
print(json.dumps(manifest, indent=2), flush=True)
shutil.rmtree(WORK)
