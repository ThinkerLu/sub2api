#!/usr/bin/env python3
"""Deploy or roll back the v0.2.9-beta1 WS image on ac.thinkerlu.top only."""

import argparse
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import socket
import subprocess
import time
import uuid


HERE = Path(__file__).resolve().parent
DEPLOY = Path('/home/thinkerlu/sub2api-deploy')
BASE_IMAGE = 'sub2api:v0.2.8-beta3-adaptive-ws-keepalive-b3a100e05'
BASE_COMMIT = 'b3a100e05054b265a2e0a8a71827c659553503e9'
BASE_ID = 'sha256:84f6a16649c84907f523d55821d07c5903422565e6cbfe97c0c2ceac41b21ad6'
BASE_CONFIG_ID = 'sha256:24f939378b80e8a1f24bfa0d01a1a87d896441417f2c10f35966640708d10e21'
BASE_BINARY_SHA256 = '3493620575780e86f888ae5dad02da04f2fa8424ae8a1ca835eb0dac906a1943'
BASE_IDS = {BASE_ID, BASE_CONFIG_ID}
POINTER = DEPLOY / '.ws-standalone-v029-beta1-main-backup-path'
VERSION = 'v0.2.9-beta1-ws-standalone-named-output'
BACKUP_PREFIX = 'ws-standalone-v029-beta1-main-'
IMAGE_BACKUP = 'old-image.tar.gz'


def run(args, *, output=None, input_text=None):
    result = subprocess.run(args, input=input_text, text=output is None,
                            stdout=output if output is not None else subprocess.PIPE,
                            stderr=subprocess.PIPE)
    if result.returncode:
        raise RuntimeError('命令失败（详细输出已隐藏）：' + ' '.join(args[:3]))
    return result.stdout if output is None else ''


def docker(*args):
    return run(['docker', *args])


def compose(*args, input_text=None):
    return run(['docker', 'compose', '-f', str(DEPLOY / 'docker-compose.yml'), *args], input_text=input_text)


def digest(path):
    sha = hashlib.sha256()
    with Path(path).open('rb') as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b''):
            sha.update(chunk)
    return sha.hexdigest()


def backup_root():
    return DEPLOY / 'backups'


def cleanup_incomplete_backups():
    root = backup_root()
    if not root.is_dir():
        return []
    pointed = None
    if POINTER.is_file():
        pointed = Path(POINTER.read_text().strip()).resolve()
    removed = []
    for path in root.iterdir():
        if path.name.startswith(BACKUP_PREFIX) and path.is_dir() and path.resolve() != pointed:
            if not (path / 'backup.json').is_file():
                shutil.rmtree(path)
                removed.append(path.name)
    return removed


def save_base_image(backup, *, skip_image_backup=False):
    if skip_image_backup:
        return None
    archive = backup / IMAGE_BACKUP
    run(['bash', '-o', 'pipefail', '-c', 'docker save "$1" | gzip -1 > "$2"',
         'bash', BASE_IMAGE, str(archive)])
    assert archive.is_file() and archive.stat().st_size > 0, '旧镜像备份为空'
    run(['gzip', '-t', str(archive)])
    return archive


def ensure_new_image(manifest):
    try:
        info = json.loads(docker('image', 'inspect', manifest['image']))[0]
    except RuntimeError:
        info = None
    if info is not None:
        assert (info['Os'], info['Architecture']) == ('linux', 'amd64'), '已加载的新镜像架构不符'
        assert info['Id'] in (manifest['image_id'], manifest['image_config_digest']), '已加载的新镜像摘要不匹配'
        assert manifest['commit'] in docker('run', '--rm', '--pull', 'never', '--network', 'none',
                                            '--entrypoint', '/app/sub2api', manifest['image'], '--version')
        return
    docker('load', '-i', str(HERE / manifest['archive']))
    assert image_id(manifest['image']) in (manifest['image_id'], manifest['image_config_digest']), '新镜像摘要不匹配'
    assert manifest['commit'] in docker('run', '--rm', '--pull', 'never', '--network', 'none',
                                        '--entrypoint', '/app/sub2api', manifest['image'], '--version')


def verify_package(manifest):
    assert manifest['target'] == 'ac.thinkerlu.top', '发布目标不匹配'
    assert manifest['rollback_commit'] == BASE_COMMIT and manifest['rollback_image'] == BASE_IMAGE
    assert manifest['rollback_image_id'] == BASE_ID
    assert manifest['rollback_image_config_digest'] == BASE_CONFIG_ID
    assert manifest['rollback_binary_sha256'] == BASE_BINARY_SHA256
    assert manifest['release_branch'] == 'codex/release-sub2api'
    assert re.fullmatch(r'[0-9a-f]{40}', manifest['commit'])
    assert manifest['version'] == VERSION, '发布版本不匹配'
    assert manifest['image'] == 'sub2api:' + manifest['version'] + '-' + manifest['commit'][:9], '镜像标签与发布版本或源码提交不一致'
    assert manifest['image'].endswith(manifest['commit'][:9]), '镜像标签与源码提交不一致'
    assert Path(manifest['archive']).name == manifest['archive'], '镜像归档名称不合法'
    checked = set()
    for line in (HERE / 'SHA256SUMS').read_text().splitlines():
        expected, name = line.split('  ', 1)
        relative = Path(name)
        file = HERE / relative
        assert re.fullmatch(r'[0-9a-f]{64}', expected) and not relative.is_absolute() and '..' not in relative.parts
        assert relative.as_posix() == name and not file.is_symlink() and HERE.resolve() in file.resolve().parents
        assert name not in checked and file.is_file() and digest(file) == expected, '发布文件 SHA256 不匹配: ' + name
        checked.add(name)
    assert {'manifest.json', 'deploy.sh', 'rollback.sh', 'release.py', 'README.md', manifest['archive']} <= checked, '发布包不完整'
    assert manifest['database_migrations_changed'] is False
    assert manifest['additive_migrations'] == {}, '本版本不允许新增数据库迁移'
    assert os.access(HERE / 'deploy.sh', os.X_OK) and os.access(HERE / 'rollback.sh', os.X_OK), '脚本缺少可执行权限'
    archive = HERE / manifest['archive']
    assert digest(archive) == manifest['archive_sha256'], '镜像归档 SHA256 不匹配'
    return archive


def image_id(image):
    info = json.loads(docker('image', 'inspect', image))[0]
    assert (info['Os'], info['Architecture']) == ('linux', 'amd64'), '镜像架构不符'
    return info['Id']


def current_container():
    cid = compose('ps', '-a', '-q', 'sub2api').strip()
    assert cid, 'sub2api 容器不存在'
    return json.loads(docker('inspect', cid))[0]


def version():
    return compose('exec', '-T', 'sub2api', '/app/sub2api', '--version')


def migrations():
    return compose('exec', '-T', 'postgres', 'sh', '-ec',
                   'exec psql -X -U "$POSTGRES_USER" -d "${POSTGRES_DB:-$POSTGRES_USER}" -At -v ON_ERROR_STOP=1',
                   input_text='SELECT filename, checksum FROM schema_migrations ORDER BY filename;\n')


def migration_map(snapshot):
    rows = {}
    for line in snapshot.splitlines():
        if not line.strip():
            continue
        name, checksum = line.split('|', 1)
        assert name not in rows and re.fullmatch(r'[0-9a-f]{64}', checksum), '迁移记录格式不合法'
        rows[name] = checksum
    return rows


def compatible_migrations(before, after, manifest):
    old, new = migration_map(before), migration_map(after)
    assert manifest.get('database_migrations_changed') is False, '发布包迁移声明不匹配'
    assert manifest.get('additive_migrations') == {}, '发布包迁移声明不匹配'
    assert old == new, '迁移记录发生变化，拒绝部署或回滚'


def drain_recovery_window():
    environment = json.loads(compose('config', '--format', 'json'))['services']['sub2api'].get('environment', {})
    database = int(environment.get('REDIS_DB', '0'))
    assert 0 <= database <= 1024, 'Redis DB 配置不合法'
    compose('stop', '-t', '30', 'sub2api')
    deadline = time.monotonic() + 130
    while True:
        pending = compose('exec', '-T', 'redis', 'redis-cli', '-n', str(database), '--scan', '--pattern', 'openai:ws-recovery:v1:migration:*').strip()
        replay = compose('exec', '-T', 'redis', 'redis-cli', '-n', str(database), '--scan', '--pattern', 'openai:ws-recovery:v1:replay:*').strip()
        if not pending and not replay:
            return
        assert time.monotonic() < deadline, '迁移窗口未排空，保留应用停止状态并拒绝旧版接管'
        time.sleep(2)


def healthy(commit, binary_sha256=None):
    for _ in range(60):
        state = current_container()['State']
        if state.get('Health', {}).get('Status') == 'healthy':
            assert commit in version(), '运行版本不匹配'
            expected = binary_sha256 or (BASE_BINARY_SHA256 if commit == BASE_COMMIT else None)
            assert expected and compose('exec', '-T', 'sub2api', 'sha256sum', '/app/sub2api').split()[0] == expected, '运行二进制摘要不匹配'
            compose('exec', '-T', 'sub2api', 'wget', '-q', '-T', '5', '-O', '/dev/null', 'http://127.0.0.1:8080/health')
            run(['curl', '-fsS', '--connect-timeout', '5', '--max-time', '15',
                 'https://subapi.thinkerlu.top/health'], output=subprocess.DEVNULL)
            return
        if not state.get('Running') and not state.get('Restarting'):
            break
        time.sleep(2)
    raise RuntimeError('应用未通过健康检查')


def compose_image():
    return json.loads(compose('config', '--format', 'json'))['services']['sub2api']['image']


def switched_compose(text, old, new):
    lines = text.splitlines(keepends=True)
    start = next((i for i, line in enumerate(lines) if re.fullmatch(r'  sub2api:\s*\n?', line)), None)
    assert start is not None, 'Compose 缺少 sub2api 服务'
    end = next((i for i in range(start + 1, len(lines))
                if re.match(r'^\S|^  [A-Za-z0-9_-]+:', lines[i])), len(lines))
    block = ''.join(lines[start + 1:end])
    updated, count = re.subn(r'(?m)^    image: ' + re.escape(old) + r'[ \t]*$', '    image: ' + new, block)
    assert count == 1, 'Compose 中的基线镜像不匹配'
    return ''.join(lines[:start + 1]) + updated + ''.join(lines[end:])


def atomic(path, text):
    temp = Path(str(path) + '.ws-standalone-v029-beta1.tmp')
    temp.write_text(text)
    original = path.stat() if path.exists() else None
    temp.chmod(original.st_mode & 0o777 if original else 0o600)
    if original and (temp.stat().st_uid, temp.stat().st_gid) != (original.st_uid, original.st_gid):
        os.chown(temp, original.st_uid, original.st_gid)
    temp.replace(path)


def check_preflight(manifest, *, skip_database_backup=False, skip_image_backup=False):
    verify_package(manifest)
    assert DEPLOY.is_dir() and (DEPLOY / '.env').is_file(), 'Compose 目录不完整'
    compose('config', '--quiet')
    state = current_container()
    assert state['Config'].get('Labels', {}).get('com.docker.compose.project.config_files') == str(DEPLOY / 'docker-compose.yml'), '当前服务不是已核对的单 Compose 配置'
    assert state['Config']['Image'] == BASE_IMAGE, '容器并非目标基线镜像'
    assert state['Image'] in BASE_IDS and image_id(BASE_IMAGE) == state['Image'], '旧镜像摘要不符'
    assert compose_image() == BASE_IMAGE and BASE_COMMIT in version(), '配置或运行提交不匹配'
    healthy(BASE_COMMIT)
    baseline = migrations()
    assert baseline.strip(), '迁移记录为空'
    compatible_migrations(baseline, baseline, manifest)
    assert compose('exec', '-T', 'redis', 'redis-cli', 'PING').strip() == 'PONG', 'Redis 不可用'
    if skip_database_backup:
        print('警告：显式跳过数据库备份；本次发布无法提供数据库灾难恢复。配置和旧镜像仍备份。', flush=True)
    if skip_image_backup:
        print('警告：显式跳过旧镜像归档；回滚依赖本机继续保留的 beta3 镜像 tag 和摘要。', flush=True)
    free = shutil.disk_usage(DEPLOY).free
    print(f'磁盘可用空间：{free} 字节；不设置空间准入门槛。实际写入、备份或镜像导入失败仍会中止，磁盘耗尽可能影响运行服务。', flush=True)
    return baseline


def restore(backup, manifest):
    meta = json.loads((backup / 'backup.json').read_text())
    assert meta['new_commit'] == manifest['commit'] and meta['old_commit'] == BASE_COMMIT
    image_backup = meta.get('old_image_backup', IMAGE_BACKUP)
    if image_backup == 'local-tag-retained':
        assert meta.get('old_image_id') == BASE_ID, '本机保留的旧镜像摘要记录不匹配'
    else:
        archive = backup / image_backup
        assert archive.parent == backup and archive.name == IMAGE_BACKUP, '旧镜像备份路径不合法'
        assert digest(archive) == meta['old_image_sha256'], '旧镜像备份损坏'
        run(['bash', '-o', 'pipefail', '-c', 'gzip -dc "$1" | docker load', 'bash', str(archive)])
    assert image_id(BASE_IMAGE) in BASE_IDS, '回滚镜像摘要不匹配'
    compatible_migrations((backup / 'migrations.before.txt').read_text(), migrations(), manifest)
    drain_recovery_window()
    atomic(DEPLOY / '.env', (backup / '.env').read_text())
    atomic(DEPLOY / 'docker-compose.yml', (backup / 'docker-compose.yml').read_text())
    compose('config', '--quiet')
    compose('up', '-d', '--no-deps', '--no-build', '--pull', 'never', 'sub2api')
    healthy(BASE_COMMIT)


def backup_database(backup, *, skip_database_backup=False):
    if skip_database_backup:
        return {'database_backup': 'skipped_by_operator', 'database_sha256': None}
    with (backup / 'database.dump').open('wb') as output:
        run(['docker', 'compose', '-f', str(DEPLOY / 'docker-compose.yml'), 'exec', '-T', 'postgres', 'sh', '-ec',
             'exec pg_dump -Fc -U "$POSTGRES_USER" -d "${POSTGRES_DB:-$POSTGRES_USER}"'], output=output)
    assert (backup / 'database.dump').stat().st_size > 0, '数据库备份为空'
    return {'database_backup': 'completed', 'database_sha256': digest(backup / 'database.dump')}


def deploy(manifest, *, skip_database_backup=False, skip_image_backup=False):
    baseline = check_preflight(manifest, skip_database_backup=skip_database_backup,
                               skip_image_backup=skip_image_backup)
    for name in cleanup_incomplete_backups():
        print('已清理未完成的旧备份：' + name, flush=True)
    ensure_new_image(manifest)
    assert not POINTER.exists(), '已有该发布的回滚记录，拒绝覆盖'
    stamp = time.strftime('%Y%m%d-%H%M%S') + '-' + uuid.uuid4().hex[:6]
    backup = backup_root() / (BACKUP_PREFIX + stamp)
    backup.mkdir(parents=True, mode=0o700)
    for name in ('.env', 'docker-compose.yml'):
        shutil.copy2(DEPLOY / name, backup / name)
        (backup / name).chmod(0o600)
    (backup / 'migrations.before.txt').write_text(baseline)
    database_meta = backup_database(backup, skip_database_backup=skip_database_backup)
    image_backup = save_base_image(backup, skip_image_backup=skip_image_backup)
    meta = {'old_commit': BASE_COMMIT, 'new_commit': manifest['commit'],
            'old_image_backup': image_backup.name if image_backup else 'local-tag-retained',
            'old_image_sha256': digest(image_backup) if image_backup else None,
            'old_image_id': image_id(BASE_IMAGE), **database_meta}
    (backup / 'backup.json').write_text(json.dumps(meta, indent=2) + '\n')
    atomic(POINTER, str(backup) + '\n')
    try:
        drain_recovery_window()
        original = (backup / 'docker-compose.yml').read_text()
        atomic(DEPLOY / 'docker-compose.yml', switched_compose(original, BASE_IMAGE, manifest['image']))
        compose('config', '--quiet')
        compose('up', '-d', '--no-deps', '--no-build', '--pull', 'never', 'sub2api')
        healthy(manifest['commit'], manifest['binary_sha256'])
        compatible_migrations(baseline, migrations(), manifest)
        run(['curl', '-fsS', '--connect-timeout', '5', '--max-time', '15', 'https://subapi.thinkerlu.top/health'], output=subprocess.DEVNULL)
    except BaseException:
        print('部署未通过，正在恢复旧应用；数据库业务数据保持原样。', flush=True)
        restore(backup, manifest)
        raise
    print('部署完成；回滚备份：' + str(backup), flush=True)


def rollback(manifest):
    verify_package(manifest)
    backup = Path(POINTER.read_text().strip()).resolve()
    assert backup.parent == DEPLOY / 'backups' and backup.name.startswith('ws-standalone-v029-beta1-main-'), '回滚路径不合法'
    assert compose_image() in (manifest['image'], BASE_IMAGE), '当前 Compose 版本不属于此发布'
    current = current_container()
    image = current['Config']['Image']
    assert image in (manifest['image'], BASE_IMAGE), '当前容器不属于此发布'
    allowed_ids = BASE_IDS if image == BASE_IMAGE else {manifest['image_id'], manifest['image_config_digest']}
    assert current['Image'] in allowed_ids, '当前容器镜像摘要不匹配'
    expected_commit = BASE_COMMIT if image == BASE_IMAGE else manifest['commit']
    assert expected_commit in docker('run', '--rm', '--pull', 'never', '--network', 'none', '--entrypoint', '/app/sub2api', image, '--version'), '当前镜像来源不匹配'
    stamp = time.strftime('%Y%m%d-%H%M%S')
    for name in ('.env', 'docker-compose.yml'):
        shutil.copy2(DEPLOY / name, backup / (name + '.before-rollback-' + stamp))
    restore(backup, manifest)
    print('已恢复 v0.2.8-beta3-adaptive-ws-keepalive；业务数据保留。', flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=['preflight', 'deploy', 'rollback'])
    parser.add_argument('--skip-database-backup', action='store_true',
                        help='显式跳过数据库备份，仍备份配置和旧镜像；不提供数据库灾难恢复')
    parser.add_argument('--skip-image-backup', action='store_true',
                        help='显式跳过旧镜像归档；回滚依赖本机继续保留的 beta3 镜像 tag 和摘要')
    args = parser.parse_args()
    if args.action == 'rollback' and args.skip_database_backup:
        parser.error('回滚不接受 --skip-database-backup；回滚始终保留业务数据库')
    assert os.geteuid() == 0, '需要在主服务器使用 sudo 运行'
    assert socket.gethostname() == 'vm75142.akkocloud.com', '不是已核对的主服务器'
    os.umask(0o077)
    manifest = json.loads((HERE / 'manifest.json').read_text())
    with (DEPLOY / '.sub2api-release.lock').open('a') as global_lock, (DEPLOY / '.traffic-trend-v029.lock').open('a') as baseline_lock, (DEPLOY / '.ws-standalone-v029-beta1-main.lock').open('a') as lock:
        fcntl.flock(global_lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        fcntl.flock(baseline_lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        if args.action == 'preflight':
            check_preflight(manifest, skip_database_backup=args.skip_database_backup,
                            skip_image_backup=args.skip_image_backup)
            print('主服务器预检通过，未切换服务。')
        elif args.action == 'deploy':
            deploy(manifest, skip_database_backup=args.skip_database_backup,
                   skip_image_backup=args.skip_image_backup)
        else:
            rollback(manifest)


if __name__ == '__main__':
    main()
