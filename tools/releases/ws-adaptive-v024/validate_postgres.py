#!/usr/bin/env python3
"""本地真实 PostgreSQL/Redis 演练；不使用生产配置或映射端口。"""
import json
import os
from pathlib import Path
import subprocess
import tempfile
import time
import uuid
from unittest.mock import patch
import release

DOCKER = ['docker']
if os.environ.get('DOCKER_CONTEXT'):
    DOCKER += ['--context', os.environ['DOCKER_CONTEXT']]

def run(*args, data=None):
    return subprocess.check_output(DOCKER + list(args), input=data, text=True)

pg, redis = None, None
try:
    pg = run('run', '-d', '--network', 'none', '--tmpfs', '/var/lib/postgresql', '-e', 'POSTGRES_PASSWORD=local-test-only', 'postgres:18-alpine').strip()
    redis = run('run', '-d', '--network', 'none', 'redis:8-alpine').strip()
    for _ in range(30):
        if subprocess.run(DOCKER + ['exec', pg, 'pg_isready', '-U', 'postgres'], capture_output=True).returncode == 0:
            break
        time.sleep(1)
    def sql(query):
        return run('exec', '-i', pg, 'psql', '-X', '-q', '-U', 'postgres', '-At', '-v', 'ON_ERROR_STOP=1', data=query)
    sql("CREATE TABLE usage_logs (id bigint, request_type smallint, stream boolean, openai_ws_mode boolean); INSERT INTO usage_logs VALUES (1, 3, true, true);")
    migration = (Path(__file__).parent / '238_usage_log_upstream_transport.sql').read_text()
    sql(migration)
    sql(migration)  # 幂等
    assert sql('SELECT upstream_transport FROM usage_logs;').strip() == '0'
    sql('UPDATE usage_logs SET upstream_transport = 1;')
    # 旧版列列表照常读写，新增列和已记录值保留。
    sql('INSERT INTO usage_logs (id, request_type, stream, openai_ws_mode) VALUES (2, 3, true, true);')
    assert sql('SELECT upstream_transport FROM usage_logs ORDER BY id;').strip() == '1\n0'
    sql("CREATE TABLE accounts (id bigint PRIMARY KEY, platform text, extra jsonb, updated_at timestamptz); CREATE TABLE scheduler_outbox (event_type text, account_id bigint);")
    oauth, apikey = release.MODE_KEYS
    rows = [(1, 'openai', {oauth: 'adaptive', apikey: 'off', 'keep': 42}),
            (2, 'openai', {apikey: ' Adaptive '}),
            (3, 'openai', {oauth: 'passthrough'}),
            (4, 'other', {oauth: 'adaptive'})]
    for ident, platform, extra in rows:
        sql("INSERT INTO accounts VALUES (%d, '%s', '%s'::jsonb, NOW());" % (ident, platform, json.dumps(extra)))
    for ident in (1, 2, 3, 4):
        run('exec', redis, 'redis-cli', 'SET', 'sched:acc:' + str(ident), 'synthetic')
    with tempfile.TemporaryDirectory() as tmp:
        r = object.__new__(release.Release)
        r.sql = sql
        r.config = lambda: {'environment': {'REDIS_DB': '0'}}
        r.dc = lambda *args, data=None: run('exec', redis, *args[3:])
        release.Release.normalize_modes(r, Path(tmp))
        saved = json.loads((Path(tmp) / 'adaptive-modes.before-rollback.json').read_text())
        assert {row['id'] for row in saved} == {1, 2}
        release.Release.normalize_modes(r, Path(tmp))
    assert json.loads(sql('SELECT extra FROM accounts WHERE id = 1;')) == {oauth: 'passthrough', apikey: 'off', 'keep': 42}
    assert json.loads(sql('SELECT extra FROM accounts WHERE id = 2;')) == {apikey: 'passthrough'}
    assert json.loads(sql('SELECT extra FROM accounts WHERE id = 4;')) == {oauth: 'adaptive'}
    assert sql('SELECT COUNT(*) FROM scheduler_outbox;').strip() == '2'
    assert run('exec', redis, 'redis-cli', 'EXISTS', 'sched:acc:1', 'sched:acc:2').strip() == '0'
    assert run('exec', redis, 'redis-cli', 'EXISTS', 'sched:acc:3', 'sched:acc:4').strip() == '2'
    print('PASS: PostgreSQL additive migration/old-column compatibility; targeted account mode rollback; Redis cache invalidation; retry idempotency.')
finally:
    for cid in (pg, redis):
        if cid:
            run('rm', '-f', '-v', cid)
