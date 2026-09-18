#!/usr/bin/env python3
"""Build the committed frontend/backend and Linux amd64 v0.2.5 image."""
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
REPO = Path('/Users/thinkerlu/Documents/apiProxy/sub2api/ws-adaptive-http-bridge')
COMMIT = os.environ["SUB2API_RELEASE_COMMIT"]
BASE_COMMIT = "86f93c28ee34cc74b629dafb748bd5ac5ca8c5ea"
VERSION = os.environ.get("SUB2API_RELEASE_VERSION", "v0.2.5-ws-preflight-fallback")
IMAGE_TAG = os.environ.get("SUB2API_RELEASE_IMAGE_TAG", "v0.2.5-ws-preflight-fallback")
IMAGE = "sub2api:" + IMAGE_TAG + "-" + COMMIT[:9]
BASE_IMAGE = "sub2api:oom-fix-ae78db6d3"
BASE_IMAGE_ID = "sha256:df0b67a341f6ddddc579f9d75e5d2e789f4eb0a7dbd8fbc2cf61a04aae40ef29"
GO = os.environ.get("SUB2API_GO", "/tmp/sub2api-oom-toolchain127/go/bin/go")
DOCKER = ["/opt/homebrew/bin/docker", "--context", "colima-sub2api-build"]
OUT = Path(os.environ.get("SUB2API_RELEASE_OUTPUT", str(HERE / "image")))
OUT.mkdir(exist_ok=True)
WORK = Path(tempfile.mkdtemp(prefix="sub2api-v025-build-", dir="/tmp"))
ENV = {**os.environ, "PATH": "/opt/homebrew/bin:" + os.environ["PATH"]}
GO_ENV = {**ENV, "CGO_ENABLED": "0", "GOOS": "linux", "GOARCH": "amd64", "GOTOOLCHAIN": "local"}


def sha(path):
    with Path(path).open("rb") as f:
        return hashlib.file_digest(f, "sha256").hexdigest()


def sha_trimmed(path):
    # Mirror repository.migrationChecksum: hash strings.TrimSpace(file).
    return hashlib.sha256(Path(path).read_text().strip().encode()).hexdigest()


def run(args, log, cwd=WORK, env=ENV):
    print("RUN", " ".join(map(str, args)), flush=True)
    result = subprocess.run(list(map(str, args)), cwd=cwd, env=env, text=True,
                            stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    (OUT / log).write_text(result.stdout)
    if result.returncode:
        print(result.stdout[-5000:], flush=True)
        raise RuntimeError(f"Command failed ({result.returncode}); see {OUT / log}")
    return result.stdout


run(["git", "merge-base", "--is-ancestor", BASE_COMMIT, COMMIT], "ancestry-baseline.log", REPO)
run(["git", "diff", "--exit-code", BASE_COMMIT, COMMIT, "--", "Dockerfile", "deploy/docker-entrypoint.sh"], "runtime-compatibility.log", REPO)
base = json.loads(run(DOCKER + ["image", "inspect", BASE_IMAGE], "base-image.json"))[0]
assert base["Id"] == BASE_IMAGE_ID and base["Architecture"] == "amd64" and base["Os"] == "linux"
run(["git", "archive", "--format=tar", "-o", WORK / "source.tar", COMMIT], "source-export.log", REPO)
source = WORK / "source"
source.mkdir()
with tarfile.open(WORK / "source.tar") as f:
    f.extractall(source, filter="data")
dist = source / "backend/internal/web/dist"
if dist.exists():
    shutil.rmtree(dist)
# Build the frontend from the exact exported commit using the locked dependencies.
shutil.copytree(REPO / "frontend/node_modules", source / "frontend/node_modules", symlinks=True)
node_env = {**ENV, "PATH": "/Users/thinkerlu/.asdf/installs/nodejs/18.14.1/bin:" + ENV["PATH"]}
run(["npx", "--yes", "pnpm@10.34.5", "run", "build"], "frontend-build.log", source / "frontend", node_env)
assert (dist / "index.html").is_file(), "Frontend build output missing"

build_date = os.environ.get("SUB2API_BUILD_DATE") or datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
context = WORK / "image"
context.mkdir()
binary = context / "sub2api"
ldflags = f"-s -w -X main.Version={VERSION} -X main.Commit={COMMIT} -X main.Date={build_date} -X main.BuildType=release"
run([GO, "build", "-p", "2", "-tags", "embed", "-trimpath", "-ldflags", ldflags, "-o", binary, "./cmd/server"],
    "backend-build.log", source / "backend", GO_ENV)
run([GO, "version", "-m", binary], "go-build-info.txt")
binary_sha = sha(binary)
shutil.copytree(source / "backend/resources", context / "resources")
shutil.copy2(REPO / "deploy/model-registry.example.yaml", OUT / "model-registry.example.yaml")
dockerfile = f'FROM {BASE_IMAGE}\nLABEL org.opencontainers.image.revision="{COMMIT}"\nLABEL org.opencontainers.image.version="{VERSION}"\nCOPY --chown=1000:1000 sub2api /app/sub2api\nCOPY --chown=1000:1000 resources /app/resources\n'
(context / "Dockerfile").write_text(dockerfile)
(OUT / "Dockerfile").write_text(dockerfile)
run(DOCKER + ["build", "--platform", "linux/amd64", "--pull=false", "--provenance=false", "-t", IMAGE, context], "image-build.log")
inspection = json.loads(run(DOCKER + ["image", "inspect", IMAGE], "image-inspect.json"))[0]
assert inspection["Architecture"] == "amd64" and inspection["Os"] == "linux"
version = run(DOCKER + ["run", "--rm", "--pull", "never", "--platform", "linux/amd64", "--network", "none", "--entrypoint", "/app/sub2api", IMAGE, "--version"], "image-version.txt")
assert COMMIT in version and VERSION in version

cid = run(DOCKER + ["create", "--platform", "linux/amd64", "--network", "none", IMAGE], "extract-container.log").strip()
try:
    run(DOCKER + ["cp", f"{cid}:/app/sub2api", WORK / "image-binary"], "extract-binary.log")
    assert sha(WORK / "image-binary") == binary_sha
    run(DOCKER + ["cp", f"{cid}:/app/resources", WORK / "image-resources"], "extract-resources.log")
    for file in (context / "resources").rglob("*"):
        if file.is_file():
            assert sha(file) == sha(WORK / "image-resources" / file.relative_to(context / "resources"))
finally:
    run(DOCKER + ["rm", "-v", cid], "extract-cleanup.log")

checks = WORK / "checks"
checks.mkdir()
run([GO, "test", "-c", "-o", checks / "relay.test", "./internal/service/openai_ws_v2"], "linux-test-build.log", source / "backend", GO_ENV)
# Copy into the container: Colima does not necessarily share macOS /tmp.
cid = run(DOCKER + ["create", "--pull", "never", "--platform", "linux/amd64", "--network", "none", "--entrypoint", "/tmp/relay.test", IMAGE,
                   "-test.run", "TestRelay_FirstTokenMs_PerTurnWithoutResponseID|TestObserveUpstreamMessage_FirstTokenMs_IndependentOfConnection|TestRelaySingleTurnPreservesDownstreamAndPendingRequest", "-test.v"], "regression-container.log").strip()
try:
    run(DOCKER + ["cp", checks / "relay.test", f"{cid}:/tmp/relay.test"], "regression-copy.log")
    run(DOCKER + ["start", "-a", cid], "linux-amd64-regression.log")
    state = json.loads(run(DOCKER + ["inspect", cid], "regression-inspect.json"))[0]["State"]
    assert state["ExitCode"] == 0 and not state["OOMKilled"], state
finally:
    run(DOCKER + ["rm", "-f", "-v", cid], "regression-cleanup.log")

# Verify socket reuse with the real Linux WebSocket implementation.
run([GO, "test", "-c", "-o", checks / "adaptive.test", "./internal/service"], "linux-adaptive-test-build.log", source / "backend", GO_ENV)
cid = run(DOCKER + ["create", "--pull", "never", "--platform", "linux/amd64", "--network", "none", "--entrypoint", "/tmp/adaptive.test", IMAGE,
                   "-test.run", "TestAdaptiveWarmupReusesRealWebSocket|TestAdaptiveRepeatedWarmupsRetainSocketUntilBusiness", "-test.v"], "adaptive-container.log").strip()
try:
    run(DOCKER + ["cp", checks / "adaptive.test", f"{cid}:/tmp/adaptive.test"], "adaptive-copy.log")
    run(DOCKER + ["start", "-a", cid], "linux-amd64-adaptive-regression.log")
    state = json.loads(run(DOCKER + ["inspect", cid], "adaptive-inspect.json"))[0]["State"]
    assert state["ExitCode"] == 0 and not state["OOMKilled"], state
finally:
    run(DOCKER + ["rm", "-f", "-v", cid], "adaptive-cleanup.log")

cid = run(DOCKER + ["run", "-d", "--pull", "never", "--platform", "linux/amd64", "--network", "none", "--no-healthcheck", IMAGE], "smoke-container.log").strip()
try:
    for _ in range(20):
        r = subprocess.run(DOCKER + ["exec", cid, "wget", "-q", "-T", "2", "-O", "-", "http://127.0.0.1:8080/"], env=ENV, capture_output=True, text=True)
        if r.returncode == 0 and '<div id="app">' in r.stdout:
            (OUT / "smoke-http.txt").write_text("GET / returned the embedded frontend successfully (setup mode, no database).\n")
            break
        time.sleep(1)
    else:
        raise RuntimeError("Embedded frontend smoke check failed")
finally:
    run(DOCKER + ["logs", cid], "smoke.log")
    run(DOCKER + ["stop", "-t", "10", cid], "smoke-stop.log")
    state = json.loads(run(DOCKER + ["inspect", cid], "smoke-inspect.json"))[0]["State"]
    (OUT / "smoke-state.json").write_text(json.dumps(state, indent=2))
    run(DOCKER + ["rm", "-v", cid], "smoke-cleanup.log")
    assert not state["OOMKilled"]

raw = WORK / "image.tar"
run(DOCKER + ["save", "-o", raw, IMAGE], "image-save.log")
archive = OUT / f"sub2api-{IMAGE_TAG}-{COMMIT[:9]}.tar.gz"
with raw.open("rb") as src, archive.open("wb") as dst, gzip.GzipFile(filename="", mode="wb", fileobj=dst, compresslevel=6, mtime=0) as gz:
    shutil.copyfileobj(src, gz, 1024 * 1024)
with gzip.open(archive, "rb") as f:
    while f.read(1024 * 1024):
        pass
with tarfile.open(raw) as image_tar:
    docker_manifest = json.load(image_tar.extractfile('manifest.json'))
    config_bytes = image_tar.extractfile(docker_manifest[0]['Config']).read()
    config_digest = 'sha256:' + hashlib.sha256(config_bytes).hexdigest()
archive_sha = sha(archive)
(OUT / (archive.name + ".sha256")).write_text(archive_sha + "  " + archive.name + "\n")
manifest = dict(commit=COMMIT, base_commit=BASE_COMMIT, version=VERSION, image=IMAGE,
                image_id=inspection["Id"], image_config_digest=config_digest, platform="linux/amd64", runtime_base=BASE_IMAGE,
                runtime_base_id=BASE_IMAGE_ID, build_date=build_date, binary_sha256=binary_sha,
                archive=archive.name, archive_sha256=archive_sha, archive_bytes=archive.stat().st_size,
                retained_commits=["0aee16183", "17866c78b"],
                upstream_tag="v0.2.5", frontend_source_commit=COMMIT,
                migration_238_opencode_go_platform_sha256=sha_trimmed(source / "backend/migrations/238_opencode_go_platform.sql"),
                migration_238_purge_unlimited_user_platform_quotas_sha256=sha_trimmed(source / "backend/migrations/238_purge_unlimited_user_platform_quotas.sql"),
                migration_238_usage_log_upstream_transport_sha256=sha_trimmed(source / "backend/migrations/238_usage_log_upstream_transport.sql"),
                build_dir=str(WORK))
(OUT / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")
print(json.dumps(manifest, indent=2), flush=True)

shutil.rmtree(WORK)
