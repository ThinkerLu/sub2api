#!/usr/bin/env python3
"""Deploy or roll back the v0.2.9 traffic trend image on cc.thkl.site only."""

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


HERE = Path(__file__).resolve().parent
DEPLOY = Path('/home/thinkerlu/sub2api-deploy')
BASE_IMAGE = 'sub2api:v0.2.8-request-traffic-monitoring-7a3472327'
BASE_COMMIT = '7a3472327b70c196089fc07c44ec5d08a3debb62'
BASE_ID = 'sha256:f3a7f2f72199af2fe0a6921163e207dc3d8848c6575344576a4e28bd45931bba'
POINTER = DEPLOY / '.traffic-trend-v029-backup-path'


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
    return run(['docker', 'compose', '-f', str(DEPLOY / 'docker-compose.yml'),
                '-f', str(DEPLOY / 'docker-compose.host-access.yml'), *args], input_text=input_text)


def digest(path):
    sha = hashlib.sha256()
    with Path(path).open('rb') as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b''):
            sha.update(chunk)
    return sha.hexdigest()


def verify_package(manifest):
    assert manifest['target'] == 'cc.thkl.site', '发布目标不匹配'
    assert manifest['rollback_commit'] == BASE_COMMIT and manifest['rollback_image'] == BASE_IMAGE
    assert manifest['rollback_image_id'] == BASE_ID
    assert manifest['release_branch'] == 'codex/release-sub2api'
    assert re.fullmatch(r'[0-9a-f]{40}', manifest['commit'])
    assert re.fullmatch(r'sub2api:v0\.2\.9-traffic-trend-dashboard-[0-9a-f]{9}', manifest['image'])
    assert manifest['image'].endswith(manifest['commit'][:9]), '镜像标签与源码提交不一致'
    assert Path(manifest['archive']).name == manifest['archive'], '镜像归档名称不合法'
    for line in (HERE / 'SHA256SUMS').read_text().splitlines():
        expected, name = line.split('  ', 1)
        assert Path(name).name == name and digest(HERE / name) == expected, '发布文件 SHA256 不匹配: ' + name
    archive = HERE / manifest['archive']
    assert digest(archive) == manifest['archive_sha256'], '镜像归档 SHA256 不匹配'
    return archive


def image_id(image):
    info = json.loads(docker('image', 'inspect', image))[0]
    assert (info['Os'], info['Architecture']) == ('linux', 'amd64'), '镜像架构不符'
    return info['Id']


def current_container():
    cid = compose('ps', '-q', 'sub2api').strip()
    assert cid, 'sub2api 容器不存在'
    return json.loads(docker('inspect', cid))[0]


def version():
    return compose('exec', '-T', 'sub2api', '/app/sub2api', '--version')


def migrations():
    return compose('exec', '-T', 'postgres', 'sh', '-ec',
                   'exec psql -X -U "$POSTGRES_USER" -d "${POSTGRES_DB:-$POSTGRES_USER}" -At -v ON_ERROR_STOP=1',
                   input_text='SELECT filename, checksum FROM schema_migrations ORDER BY filename;\n')


def healthy(commit):
    for _ in range(60):
        state = current_container()['State']
        if state.get('Health', {}).get('Status') == 'healthy':
            assert commit in version(), '运行版本不匹配'
            run(['curl', '-fsS', '--connect-timeout', '2', '--max-time', '5',
                 'http://127.0.0.1:18080/health'], output=subprocess.DEVNULL)
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
    temp = Path(str(path) + '.traffic-trend.tmp')
    temp.write_text(text)
    temp.chmod(path.stat().st_mode & 0o777 if path.exists() else 0o600)
    temp.replace(path)


def check_preflight(manifest):
    archive = verify_package(manifest)
    assert DEPLOY.is_dir() and (DEPLOY / '.env').is_file(), 'Compose 目录不完整'
    assert (DEPLOY / 'docker-compose.host-access.yml').is_file(), '备用机覆盖文件不存在'
    compose('config', '--quiet')
    state = current_container()
    assert state['Config']['Image'] == BASE_IMAGE, '容器并非目标基线镜像'
    assert state['Image'] == BASE_ID and image_id(BASE_IMAGE) == BASE_ID, '旧镜像摘要不符'
    assert compose_image() == BASE_IMAGE and BASE_COMMIT in version(), '配置或运行提交不匹配'
    healthy(BASE_COMMIT)
    baseline = migrations()
    assert baseline.strip(), '迁移记录为空'
    assert compose('exec', '-T', 'redis', 'redis-cli', 'PING').strip() == 'PONG', 'Redis 不可用'
    size = int(compose('exec', '-T', 'postgres', 'sh', '-ec',
                       'exec psql -X -U "$POSTGRES_USER" -d "${POSTGRES_DB:-$POSTGRES_USER}" -Atqc "SELECT pg_database_size(current_database())"').strip())
    needed = int(size * 1.5) + archive.stat().st_size * 3 + 512 * 1024 * 1024
    assert shutil.disk_usage(DEPLOY).free > needed, '空间不足以备份数据库、旧镜像并导入新镜像'
    return baseline


def restore(backup, manifest):
    meta = json.loads((backup / 'backup.json').read_text())
    assert meta['new_commit'] == manifest['commit'] and meta['old_commit'] == BASE_COMMIT
    assert digest(backup / 'old-image.tar') == meta['old_image_sha256'], '旧镜像备份损坏'
    docker('load', '-i', str(backup / 'old-image.tar'))
    assert image_id(BASE_IMAGE) == BASE_ID, '回滚镜像摘要不匹配'
    assert migrations() == (backup / 'migrations.before.txt').read_text(), '迁移发生变化，拒绝自动回滚'
    atomic(DEPLOY / '.env', (backup / '.env').read_text())
    atomic(DEPLOY / 'docker-compose.yml', (backup / 'docker-compose.yml').read_text())
    atomic(DEPLOY / 'docker-compose.host-access.yml', (backup / 'docker-compose.host-access.yml').read_text())
    compose('config', '--quiet')
    compose('up', '-d', '--no-deps', '--no-build', '--pull', 'never', 'sub2api')
    healthy(BASE_COMMIT)


def deploy(manifest):
    baseline = check_preflight(manifest)
    archive = HERE / manifest['archive']
    docker('load', '-i', str(archive))
    assert image_id(manifest['image']) in (manifest['image_id'], manifest['image_config_digest']), '新镜像摘要不匹配'
    assert manifest['commit'] in docker('run', '--rm', '--pull', 'never', '--network', 'none',
                                        '--entrypoint', '/app/sub2api', manifest['image'], '--version')
    assert not POINTER.exists(), '已有该发布的回滚记录，拒绝覆盖'
    stamp = time.strftime('%Y%m%d-%H%M%S') + '-' + uuid.uuid4().hex[:6]
    backup = DEPLOY / 'backups' / ('traffic-trend-v029-' + stamp)
    backup.mkdir(parents=True, mode=0o700)
    for name in ('.env', 'docker-compose.yml', 'docker-compose.host-access.yml'):
        shutil.copy2(DEPLOY / name, backup / name)
        (backup / name).chmod(0o600)
    (backup / 'migrations.before.txt').write_text(baseline)
    with (backup / 'database.dump').open('wb') as output:
        run(['docker', 'compose', '-f', str(DEPLOY / 'docker-compose.yml'), '-f',
             str(DEPLOY / 'docker-compose.host-access.yml'), 'exec', '-T', 'postgres', 'sh', '-ec',
             'exec pg_dump -Fc -U "$POSTGRES_USER" -d "${POSTGRES_DB:-$POSTGRES_USER}"'], output=output)
    assert (backup / 'database.dump').stat().st_size > 0, '数据库备份为空'
    docker('save', '-o', str(backup / 'old-image.tar'), BASE_IMAGE)
    meta = {'old_commit': BASE_COMMIT, 'new_commit': manifest['commit'],
            'old_image_sha256': digest(backup / 'old-image.tar'), 'database_sha256': digest(backup / 'database.dump')}
    (backup / 'backup.json').write_text(json.dumps(meta, indent=2) + '\n')
    atomic(POINTER, str(backup) + '\n')
    try:
        original = (backup / 'docker-compose.yml').read_text()
        atomic(DEPLOY / 'docker-compose.yml', switched_compose(original, BASE_IMAGE, manifest['image']))
        compose('config', '--quiet')
        compose('up', '-d', '--no-deps', '--no-build', '--pull', 'never', 'sub2api')
        healthy(manifest['commit'])
        assert migrations() == baseline, '出现非预期的数据库迁移'
    except BaseException:
        print('部署未通过，正在恢复旧应用；数据库业务数据保持原样。', flush=True)
        restore(backup, manifest)
        raise
    print('部署完成；回滚备份：' + str(backup), flush=True)


def rollback(manifest):
    verify_package(manifest)
    backup = Path(POINTER.read_text().strip()).resolve()
    assert backup.parent == DEPLOY / 'backups' and backup.name.startswith('traffic-trend-v029-'), '回滚路径不合法'
    assert compose_image() in (manifest['image'], BASE_IMAGE), '当前 Compose 版本不属于此发布'
    assert manifest['commit'] in version() or BASE_COMMIT in version(), '当前版本不属于此发布'
    stamp = time.strftime('%Y%m%d-%H%M%S')
    for name in ('.env', 'docker-compose.yml', 'docker-compose.host-access.yml'):
        shutil.copy2(DEPLOY / name, backup / (name + '.before-rollback-' + stamp))
    restore(backup, manifest)
    print('已恢复 v0.2.8；数据库和业务数据未回退。', flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=['preflight', 'deploy', 'rollback'])
    args = parser.parse_args()
    assert os.geteuid() == 0, '需要在备用机使用 sudo 运行'
    os.umask(0o077)
    manifest = json.loads((HERE / 'manifest.json').read_text())
    with (DEPLOY / '.traffic-trend-v029.lock').open('a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        if args.action == 'preflight':
            check_preflight(manifest)
            print('备用机预检通过，未切换服务。')
        elif args.action == 'deploy':
            deploy(manifest)
        else:
            rollback(manifest)


if __name__ == '__main__':
    main()
