#!/usr/bin/env python3
"""v0.2.5 external model registry release, reload, and rollback."""
import argparse
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import time
import uuid

if not __debug__:
    raise RuntimeError('发布校验不允许 python -O')

HERE = Path(__file__).resolve().parent
OLD_COMMIT = '4700dbdf6d0fd0f20930cf6cfbc4adf9a91ed7bb'
OLD_IMAGE = 'sub2api:v0.2.5-model-registry-input-fastpath-r2-4700dbdf6'
POINTER = '.ws-preflight-fallback-v025-backup-path'
REGISTRY_DIR = 'model-registry'
REGISTRY_FILE = 'model-registry.yaml'
REGISTRY_HOST_DIR = './model-registry'
REGISTRY_CONTAINER_DIR = '/app/model-registry'
REGISTRY_CONTAINER_FILE = REGISTRY_CONTAINER_DIR + '/' + REGISTRY_FILE
REGISTRY_MOUNT = f'      - {REGISTRY_HOST_DIR}:{REGISTRY_CONTAINER_DIR}:ro\n'
REGISTRY_ENV = {
    'MODEL_REGISTRY_ENABLED': 'true',
    'MODEL_REGISTRY_FILE': REGISTRY_CONTAINER_FILE,
    'MODEL_REGISTRY_RELOAD_INTERVAL_SECONDS': '5',
}
REGISTRY_COMPOSE_ENV = {
    'MODEL_REGISTRY_ENABLED': '${MODEL_REGISTRY_ENABLED:-true}',
    'MODEL_REGISTRY_FILE': '${MODEL_REGISTRY_FILE:-' + REGISTRY_CONTAINER_FILE + '}',
    'MODEL_REGISTRY_RELOAD_INTERVAL_SECONDS': '${MODEL_REGISTRY_RELOAD_INTERVAL_SECONDS:-5}',
}

# The application migration runner hashes strings.TrimSpace(file_contents),
# not raw file bytes. These values must mirror repository.migrationChecksum.
KNOWN_ADDITIVE_MIGRATIONS = {
    '238_opencode_go_platform.sql': '6f987e251519bd3759e60da44620a5d777494cceb333b6ce394aa0ea536ef5a2',
    '238_purge_unlimited_user_platform_quotas.sql': '052756d1b1ac951f002034bf0434f8a6ea943ec0a2a34541ca4374ba6278cce7',
    '238_usage_log_upstream_transport.sql': 'a3c5e3e2d7fe680601559a0d01f4e76138fa2774544510e7804217e831bc8383',
}
REQUIRED_MIGRATIONS = (
    '238_opencode_go_platform.sql',
    '238_purge_unlimited_user_platform_quotas.sql',
)


def check_migrations(before, after, require_new=False):
    def parse(text):
        rows = [line.split('|', 1) for line in text.splitlines() if line]
        result = dict(rows)
        assert len(result) == len(rows), '重复迁移记录'
        return result
    old, new = parse(before), parse(after)
    assert old, '迁移记录为空'
    for name, checksum in KNOWN_ADDITIVE_MIGRATIONS.items():
        if name in new:
            assert new[name] == checksum, f'迁移 checksum 不匹配: {name}'
    if require_new:
        missing = [name for name in REQUIRED_MIGRATIONS if name not in new]
        assert not missing, '新增迁移未执行: ' + ', '.join(missing)
    assert all(new.get(k) == v for k, v in old.items()), '已有迁移变化或丢失'
    assert set(new) - set(old) <= set(KNOWN_ADDITIVE_MIGRATIONS), '出现其他迁移，拒绝自动回滚'


def run(args, data=None):
    p = subprocess.run(args, input=data, text=True, capture_output=True)
    if p.returncode:
        # config 输出可能包含凭据，失败时仅报告命令类型。
        raise RuntimeError('命令失败: ' + ' '.join(args[:4]))
    return p.stdout


def run_combined(args, data=None):
    """Run a command and combine stdout/stderr; only used for identity checks."""
    p = subprocess.run(args, input=data, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    if p.returncode:
        raise RuntimeError('命令失败: ' + ' '.join(args[:4]))
    return p.stdout


def atomic(path, text):
    path = Path(path)
    tmp = path.with_name(path.name + '.ws-oom.tmp')
    tmp.write_text(text)
    tmp.chmod(path.stat().st_mode & 0o777 if path.exists() else 0o600)
    tmp.replace(path)


def upsert_env(text, values):
    """Update exact env keys without dropping comments or unrelated values."""
    output = []
    seen = set()
    for line in text.splitlines():
        match = re.match(r'^([A-Za-z_][A-Za-z0-9_]*)=', line)
        key = match.group(1) if match else None
        if key in values:
            if key in seen:
                raise ValueError(f'.env 包含重复配置: {key}')
            seen.add(key)
            line = key + '=' + values[key]
        output.append(line)
    for key, value in values.items():
        if key not in seen:
            output.append(key + '=' + value)
    return '\n'.join(output) + '\n'


def edit_compose(text, image, registry=True):
    """只接受已验证的标准缩进；不重新序列化或打印凭据。"""
    if not re.fullmatch(r'sub2api:[A-Za-z0-9_.-]+', image):
        raise ValueError('非法镜像标签')
    lines = text.splitlines(keepends=True)
    starts = [i for i, line in enumerate(lines) if re.fullmatch(r'  sub2api:\s*\n?', line)]
    if len(starts) != 1:
        raise ValueError('Compose 必须有一个缩进为两空格的 sub2api 服务')
    start = starts[0] + 1
    end = next((i for i in range(start, len(lines)) if re.match(r'^\S|^  [A-Za-z0-9_-]+:', lines[i])), len(lines))
    block = ''.join(lines[start:end])
    block, n = re.subn(r'(?m)^    image:.*$', '    image: ' + image, block)
    if n != 1:
        raise ValueError('sub2api 必须有唯一 image 字段')
    if registry and REGISTRY_MOUNT not in block:
        block_lines = block.splitlines(keepends=True)
        volume_header = next((i for i, line in enumerate(block_lines) if re.fullmatch(r'    volumes:\s*\n?', line)), None)
        if volume_header is not None:
            block_lines.insert(volume_header + 1, REGISTRY_MOUNT)
        else:
            insert_at = next((i for i, line in enumerate(block_lines) if re.fullmatch(r'    (environment|depends_on|networks|logging):\s*\n?', line)), len(block_lines))
            if insert_at and not block_lines[insert_at - 1].endswith('\n'):
                block_lines[insert_at - 1] += '\n'
            block_lines[insert_at:insert_at] = ['    volumes:\n', REGISTRY_MOUNT]
        block = ''.join(block_lines)
    if registry:
        block_lines = block.splitlines(keepends=True)
        environment_header = next((i for i, line in enumerate(block_lines) if re.fullmatch(r'    environment:\s*\n?', line)), None)
        if environment_header is None:
            raise ValueError('sub2api 必须有 environment 字段以启用 model registry')
        insert_at = environment_header + 1
        for key, value in REGISTRY_COMPOSE_ENV.items():
            pattern = re.compile(r'^      ' + re.escape(key) + r':')
            if any(pattern.match(line) for line in block_lines):
                continue
            block_lines.insert(insert_at, f'      {key}: "{value}"\n')
            insert_at += 1
        block = ''.join(block_lines)
    return ''.join(lines[:start]) + block + ''.join(lines[end:])


class Release:
    def __init__(self, deploy):
        self.deploy = Path(deploy).resolve()
        self.docker = ['docker'] if os.geteuid() == 0 else ['sudo', 'docker']
        self.manifest = json.loads((HERE / 'manifest.json').read_text())
        self.image = self.manifest['image']
        self.commit = self.manifest['commit']
        assert re.fullmatch(r'[0-9a-f]{40}', self.commit)
        assert re.fullmatch(r'sub2api:[A-Za-z0-9_.-]+', self.image)
        archive = self.manifest['archive']
        assert Path(archive).name == archive
        self.archive = HERE / archive
        self.compose = self.deploy / 'docker-compose.yml'
        self.env = self.deploy / '.env'
        self.bk = None

    def registry_path(self):
        return self.deploy / REGISTRY_DIR / REGISTRY_FILE

    def prepare_registry(self):
        directory = self.deploy / REGISTRY_DIR
        directory.mkdir(parents=True, exist_ok=True)
        directory.chmod(0o755)
        target = self.registry_path()
        if not target.exists():
            source = HERE / 'model-registry.example.yaml'
            if not source.is_file():
                raise RuntimeError('发布包缺少 model-registry.example.yaml')
            shutil.copy2(source, target)
            target.chmod(0o644)
        if not target.is_file() or target.stat().st_size == 0:
            raise RuntimeError('外置 model registry 文件为空')
        return target

    def d(self, *args, data=None):
        return run(self.docker + list(args), data)

    def dc(self, *args, data=None):
        return self.d('compose', '-f', str(self.compose), *args, data=data)

    def config(self):
        return json.loads(self.dc('config', '--format', 'json'))['services']['sub2api']

    def current_version(self):
        return self.dc('exec', '-T', 'sub2api', '/app/sub2api', '--version')

    def check_image(self, image, commit, expected_id=None):
        info = json.loads(self.d('image', 'inspect', image))[0]
        assert (info['Os'], info['Architecture']) == ('linux', 'amd64'), '镜像架构不匹配'
        if expected_id:
            allowed_ids = {expected_id}
            # containerd 存储返回 OCI manifest digest；传统存储返回 config digest。
            # 两个摘要均从同一 SHA256 已核验归档导出，仅本次发布镜像允许该差异。
            # 回滚快照仍严格匹配同一服务器保存的 ID。
            if image == self.image and expected_id == self.manifest['image_id']:
                config_digest = self.manifest.get('image_config_digest')
                if config_digest and re.fullmatch(r'sha256:[0-9a-f]{64}', config_digest):
                    allowed_ids.add(config_digest)
            assert info['Id'] in allowed_ids, '镜像 ID 不匹配：实际=' + info['Id'] + '；预期=' + ', '.join(sorted(allowed_ids))
        version = run_combined(self.docker + [
            'run', '--rm', '--pull', 'never', '--network', 'none',
            '--entrypoint', '/app/sub2api', image, '--version',
        ])
        assert commit in version, '程序提交不匹配'
        return info['Id']

    def migrations(self):
        return self.dc('exec', '-T', 'postgres', 'sh', '-ec',
                       'exec psql -X -U "$POSTGRES_USER" -d "${POSTGRES_DB:-$POSTGRES_USER}" -At -v ON_ERROR_STOP=1',
                       data='SELECT filename, checksum FROM schema_migrations ORDER BY filename;\n')

    def healthy(self, commit):
        for _ in range(60):
            cid = self.dc('ps', '-q', 'sub2api').strip()
            if cid:
                state = json.loads(self.d('inspect', cid))[0]['State']
                if not state.get('Running') and not state.get('Restarting'):
                    raise RuntimeError('应用容器已退出，未通过健康检查')
                if state.get('Health', {}).get('Status') == 'healthy':
                    self.dc('exec', '-T', 'sub2api', 'sh', '-ec',
                            'wget -q -T 5 -O /dev/null "http://127.0.0.1:${SERVER_PORT:-8080}/health"')
                    assert commit in self.current_version(), '实际运行版本不匹配'
                    return
            time.sleep(2)
        raise RuntimeError('120 秒内未通过健康检查；可用 docker compose logs --tail 60 sub2api 检查')

    def space(self, mib):
        root = self.d('info', '--format', '{{.DockerRootDir}}').strip()
        # Docker 数据目录通常不允许普通 SSH 用户遍历。
        usage = run(self.docker[:-1] + ['df', '-Pk', root]).splitlines()[-1].split()
        free = min(shutil.disk_usage(self.deploy).free, int(usage[3]) * 1024)
        if free < mib * 1024 * 1024:
            raise RuntimeError(f'可用磁盘仅 {free // 1024 // 1024} MiB，需要至少 {mib} MiB；尚未切换服务')

    def load_backup(self):
        raw = (self.deploy / POINTER).read_text().strip()
        bk = Path(raw).resolve()
        assert bk.parent == self.deploy / 'backups' and bk.name.startswith('ws-preflight-fallback-v025-'), '回滚目录不匹配'
        meta = json.loads((bk / 'backup.json').read_text())
        assert meta['old_commit'] == OLD_COMMIT and meta['new_commit'] == self.commit
        self.check_image(meta['rollback_image'], OLD_COMMIT, meta['rollback_id'])
        return bk, meta

    def restore(self, bk, meta):
        check_migrations((bk / 'migrations.before.txt').read_text(), self.migrations())
        self.dc('stop', '-t', '120', 'sub2api')
        atomic(self.env, (bk / '.env').read_text())
        atomic(self.compose, edit_compose((bk / 'docker-compose.yml').read_text(), meta['rollback_image'], registry=True))
        self.dc('config', '--quiet')
        self.dc('up', '-d', '--no-deps', '--no-build', '--pull', 'never', 'sub2api')
        self.healthy(OLD_COMMIT)
        check_migrations((bk / 'migrations.before.txt').read_text(), self.migrations())

    def reload(self):
        assert REGISTRY_MOUNT.strip() in self.compose.read_text(), '当前 Compose 未挂载外置 model registry'
        self.prepare_registry()
        self.dc('kill', '-s', 'HUP', 'sub2api')
        print('已发送 SIGHUP；服务会在不重启的情况下重新加载外置 model registry。', flush=True)

    def verify_registry_runtime(self):
        config = self.config()
        environment = config.get('environment', {})
        for key, value in REGISTRY_ENV.items():
            assert environment.get(key) == value, f'容器环境未生效: {key}'
        mounts = config.get('volumes', [])
        assert any(item.get('target') == REGISTRY_CONTAINER_DIR and item.get('read_only') for item in mounts), 'model registry 只读挂载未生效'
        self.dc('exec', '-T', 'sub2api', 'sh', '-ec', 'test -r "$MODEL_REGISTRY_FILE"')

    def rollback(self):
        bk, meta = self.load_backup()
        assert self.config()['image'] in (self.image, OLD_IMAGE, meta['rollback_image']), '当前配置不是本次部署版本'
        stamp = time.strftime('%Y%m%d-%H%M%S')
        shutil.copy2(self.compose, bk / ('compose.before-rollback-' + stamp + '.yml'))
        shutil.copy2(self.env, bk / ('env.before-rollback-' + stamp))
        self.restore(bk, meta)
        print('已回滚至 v0.2.5-model-registry-input-fastpath-r2 / ' + OLD_COMMIT + '；数据库、data 和 model registry 保留。', flush=True)

    def preflight(self):
        with self.archive.open('rb') as f:
            digest = hashlib.sha256()
            for block in iter(lambda: f.read(1024 * 1024), b''):
                digest.update(block)
        assert digest.hexdigest() == self.manifest['archive_sha256'], '归档 SHA256 不匹配'
        version = self.current_version()
        assert OLD_COMMIT in version or self.commit in version, '当前运行版本不是本次允许的基线或新版本'
        self.dc('config', '--quiet')
        baseline = self.migrations()
        check_migrations(baseline, baseline)
        env = self.config().get('environment', {})
        assert env.get('REDIS_HOST', 'redis') == 'redis', '脚本仅支持 Compose 内置 redis'
        db = str(env.get('REDIS_DB', '0'))
        assert db.isdigit(), '非法 Redis DB'
        assert self.dc('exec', '-T', 'redis', 'redis-cli', '-e', '--raw', '-n', db, 'PING').strip() == 'PONG', 'Redis 连接检查失败'
        self.space(512)
        print('预检通过：版本、归档、Compose、数据库、Redis、空间；未导入镜像或切换服务。')

    def install(self):
        self.preflight()
        current = self.current_version()
        if self.commit in current:
            self.healthy(self.commit)
            print('已运行本次版本，原回滚记录保留。')
            return
        assert OLD_COMMIT in current, '当前程序不是允许的 v0.2.5-model-registry-input-fastpath-r2 回滚基线'
        config = self.config()
        cid = self.dc('ps', '-q', 'sub2api').strip()
        info = json.loads(self.d('inspect', cid))[0]
        assert all(m['Destination'].rstrip('/') not in ('/app', '/app/sub2api', '/app/resources') for m in info['Mounts']), '程序或资源被挂载覆盖，不能安全快照'
        if config.get('entrypoint') or config.get('command'):
            raise RuntimeError('Compose 有自定义 entrypoint/command，需人工核对')
        self.prepare_registry()
        compose_new = edit_compose(self.compose.read_text(), self.image, registry=True)
        env_new = upsert_env(self.env.read_text(), REGISTRY_ENV)
        print('[1/5] 校验发布包和可用空间', flush=True)
        with self.archive.open('rb') as f:
            digest = hashlib.sha256()
            for block in iter(lambda: f.read(1024 * 1024), b''):
                digest.update(block)
        assert digest.hexdigest() == self.manifest['archive_sha256'], '归档 SHA256 不匹配'
        self.space(512)
        print('[2/5] 导入并验证镜像', flush=True)
        self.d('load', '-i', str(self.archive))
        self.check_image(self.image, self.commit, self.manifest['image_id'])
        self.space(128)
        # 重部署复用原快照，不覆盖第一次保存的基线。
        if (self.deploy / POINTER).exists():
            bk, meta = self.load_backup()
            check_migrations((bk / 'migrations.before.txt').read_text(), self.migrations())
        else:
            stamp = time.strftime('%Y%m%d-%H%M%S') + '-' + uuid.uuid4().hex[:6]
            bk = self.deploy / 'backups' / ('ws-preflight-fallback-v025-' + stamp)
            bk.mkdir(parents=True, mode=0o700)
            shutil.copy2(self.compose, bk / 'docker-compose.yml')
            shutil.copy2(self.env, bk / '.env')
            (bk / 'current-version.txt').write_text(current)
            stopped = False
            print('[3/5] 保存配置和当前实际程序快照', flush=True)
            try:
                stopped = True
                self.dc('stop', '-t', '120', 'sub2api')
                baseline = self.migrations()
                assert baseline.strip(), '迁移记录为空'
                (bk / 'migrations.before.txt').write_text(baseline)
                snapshot = 'sub2api:rollback-before-ws-preflight-fallback-v025-' + stamp
                snapshot_id = self.d('commit', cid, snapshot).strip()
                self.check_image(snapshot, OLD_COMMIT, snapshot_id)
                meta = dict(old_commit=OLD_COMMIT, new_commit=self.commit, rollback_image=snapshot, rollback_id=snapshot_id)
                (bk / 'backup.json').write_text(json.dumps(meta, indent=2) + '\n')
                atomic(self.deploy / POINTER, str(bk) + '\n')
            except BaseException:
                if stopped:
                    self.dc('start', 'sub2api')
                raise
        try:
            print('[4/5] 切换应用镜像并挂载外置 model registry', flush=True)
            atomic(self.env, env_new)
            atomic(self.compose, compose_new)
            self.dc('config', '--quiet')
            self.dc('up', '-d', '--no-deps', '--no-build', '--pull', 'never', 'sub2api')
            print('[5/5] 等待健康并校验程序和迁移记录', flush=True)
            self.healthy(self.commit)
            self.verify_registry_runtime()
            after = self.migrations()
            (bk / 'migrations.after.txt').write_text(after)
            check_migrations((bk / 'migrations.before.txt').read_text(), after, require_new=True)
        except BaseException:
            print('切换未通过，尝试恢复 v0.2.5-model-registry-input-fastpath-r2。', flush=True)
            self.restore(bk, meta)
            raise
        print('部署通过；回滚目录=' + str(bk), flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=['preflight', 'deploy', 'rollback', 'reload'])
    parser.add_argument('--deploy-dir', default='/home/thinkerlu/sub2api-deploy')
    args = parser.parse_args()
    os.umask(0o077)
    release = Release(args.deploy_dir)
    os.chdir(release.deploy)
    with open('.model-registry-deploy.lock', 'a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        if os.geteuid() != 0:
            subprocess.run(['sudo', '-v'], check=True)
        {'preflight': release.preflight, 'deploy': release.install, 'rollback': release.rollback, 'reload': release.reload}[args.action]()


if __name__ == '__main__':
    try:
        main()
    except (Exception, KeyboardInterrupt) as error:
        print('操作未完成: ' + str(error), flush=True)
        print('请根据上方阶段检查服务状态；若恢复中断，修复报错后重试 rollback.sh。数据库与 data 均未删除。', flush=True)
        raise SystemExit(1)
