#!/usr/bin/env python3
"""本地故障演练；不连接生产、不读取真实凭据。"""
import hashlib
import json
from pathlib import Path
import re
import tempfile
import unittest
from unittest.mock import patch

import release

NEW = '7b6ae02883ef8a904c9d8c8e9ff7c4a3661bbc41'
IMAGE = 'sub2api:v0.2.4-ws-oom-7b6ae0288'
COMPOSE = '''services:
  sub2api:
    image: sub2api:v0.2.4-memory-ttft-257eee18e
    environment:
      - AUTO_SETUP=true
      - GATEWAY_OPENAI_WS_ENABLED=true
    volumes:
      - ./data:/app/data
  postgres:
    image: postgres:18-alpine
    environment:
      - POSTGRES_USER=synthetic
'''


class FakeRelease(release.Release):
    def __init__(self, deploy):
        super().__init__(deploy)
        self.current = release.OLD_COMMIT
        self.running = True
        self.baseline = '001_initial.sql|synthetic-checksum\n'
        self.fault = None
        self.deleted = []
        self.loaded = False
        self.snapshots = 0
        self.rows = []

    def preflight(self):
        pass

    def normalize_modes(self, bk):
        self.modes_normalized = True

    def current_version(self):
        return 'Sub2API ' + self.current

    def config(self):
        text = self.compose.read_text()
        image = re.search(r'    image: (\S+)', text)[1]
        value = re.search(r'^' + release.KEY + '=(.*)$', self.env.read_text(), re.M)
        return dict(image=image, environment={release.KEY: value[1] if value else '0'})

    def migrations(self):
        return self.baseline

    def space(self, mib):
        if self.fault == 'space':
            raise RuntimeError('synthetic low disk')

    def healthy(self, commit):
        if self.fault == 'health' and commit == NEW:
            raise RuntimeError('synthetic health failure')
        assert self.current == commit and self.running

    def check_image(self, image, commit, expected_id=None):
        assert commit in (NEW, release.OLD_COMMIT)
        return 'snapshot-id'

    def d(self, *args, data=None):
        if args == ('ps', '-aq'):
            return 'cid'
        if args[0] == 'inspect':
            return json.dumps([dict(Image='current-id', Mounts=[dict(Destination='/app/data')])])
        if args[:2] == ('image', 'ls'):
            return '\n'.join(json.dumps(row) for row in self.rows)
        if args[:2] == ('image', 'rm'):
            self.deleted.append(args[2])
            return ''
        if args[0] == 'load':
            self.loaded = True
            return ''
        if args[0] == 'commit':
            if self.fault == 'backup':
                raise RuntimeError('synthetic snapshot failure')
            self.snapshots += 1
            return 'snapshot-id'
        raise AssertionError(args)

    def dc(self, *args, data=None):
        if args == ('ps', '-q', 'sub2api'):
            return 'cid'
        if args[0] == 'stop':
            self.running = False
        elif args[0] == 'start':
            self.running = True
        elif args[0] == 'config':
            pass
        elif args[0] == 'up':
            assert '--no-deps' in args and '--no-build' in args and args[args.index('--pull') + 1] == 'never'
            self.running = True
            self.current = NEW if self.config()['image'] == IMAGE else release.OLD_COMMIT
            if self.current == NEW and release.MIGRATION not in self.baseline:
                self.baseline += release.MIGRATION + '|' + release.MIGRATION_CHECKSUM + '\n'
            # 回滚旧版时不得保留本次引入的 env0 或传递项。
            if self.current == release.OLD_COMMIT:
                assert release.KEY not in self.env.read_text()
                assert release.KEY not in self.compose.read_text()
        elif args[:4] == ('exec', '-T', 'sub2api', 'printenv'):
            return self.config()['environment'][release.KEY]
        else:
            raise AssertionError(args)
        return ''


class ReleaseTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        root = Path(self.tmp.name)
        self.patch = patch.object(release, 'HERE', root)
        self.patch.start()
        self.addCleanup(self.patch.stop)
        archive = b'synthetic image'
        (root / 'image.tar.gz').write_bytes(archive)
        (root / 'manifest.json').write_text(json.dumps(dict(commit=NEW, image=IMAGE, archive='image.tar.gz',
            archive_sha256=hashlib.sha256(archive).hexdigest(), image_id='new-id')))
        deploy = root / 'deploy'
        (deploy / 'data').mkdir(parents=True)
        (deploy / 'data/business').write_text('live business data')
        (deploy / 'docker-compose.yml').write_text(COMPOSE)
        (deploy / '.env').write_text('SYNTHETIC_ONLY=1\n')
        self.r = FakeRelease(deploy)

    def test_deploy_rollback_redeploy_idempotent(self):
        r = self.r
        r.install()
        self.assertEqual(r.current, NEW)
        self.assertEqual(r.env.read_text(), 'SYNTHETIC_ONLY=1\n')
        pointer = (r.deploy / release.POINTER).read_text()
        r.install()
        r.rollback()
        self.assertEqual(r.current, release.OLD_COMMIT)
        self.assertEqual(r.env.read_text(), 'SYNTHETIC_ONLY=1\n')
        r.rollback()
        r.install()
        self.assertEqual(r.current, NEW)
        self.assertEqual(r.snapshots, 1)
        self.assertEqual((r.deploy / release.POINTER).read_text(), pointer)
        self.assertEqual((r.deploy / 'data/business').read_text(), 'live business data')
        self.assertIn(release.MIGRATION, r.baseline)
        self.assertTrue(r.modes_normalized)

    def test_failures_preserve_or_restore_old_running_app(self):
        for fault in ('space', 'backup', 'health'):
            with self.subTest(fault=fault):
                r = self.r
                r.fault = fault
                with self.assertRaises(RuntimeError):
                    r.install()
                self.assertTrue(r.running)
                self.assertEqual(r.current, release.OLD_COMMIT)
                self.assertEqual(r.env.read_text(), 'SYNTHETIC_ONLY=1\n')
                if fault in ('space', 'backup'):
                    self.assertFalse((r.deploy / release.POINTER).exists())

    def test_checksum_failure_before_load_or_stop(self):
        self.r.archive.write_bytes(b'corrupt')
        with self.assertRaises(AssertionError):
            self.r.install()
        self.assertFalse(self.r.loaded)
        self.assertTrue(self.r.running)

    def test_wrong_baseline_rejected(self):
        self.r.current = 'unexpected-version'
        with self.assertRaises(AssertionError):
            self.r.install()
        self.assertFalse(self.r.loaded)

    def test_migration_drift_blocks_rollback(self):
        self.r.install()
        self.r.baseline += '999_unexpected.sql|unknown\n'
        with self.assertRaises(AssertionError):
            self.r.rollback()
        self.assertEqual(self.r.current, NEW)
        self.assertTrue(self.r.running)

    def test_preflight_is_read_only_and_rejects_bad_redis(self):
        r = self.r
        calls = []
        def dc(*args, data=None):
            calls.append(args)
            if args[:3] == ('exec', '-T', 'redis'):
                return 'PONG\n'
            return ''
        with patch.object(r, 'dc', dc):
            release.Release.preflight(r)
        self.assertFalse(r.loaded)
        self.assertTrue(r.running)
        self.assertFalse(any(args[0] in ('stop', 'up', 'start') for args in calls))
        with patch.object(r, 'dc', lambda *args, **kwargs: 'NOAUTH'):
            with self.assertRaises(AssertionError):
                release.Release.preflight(r)

    def test_preflight_space_and_version(self):
        r = self.r
        with patch.object(r, 'dc', lambda *args, **kwargs: 'PONG'):
            r.fault = 'space'
            with self.assertRaises(RuntimeError):
                release.Release.preflight(r)
            r.fault = None
            r.current = 'wrong'
            with self.assertRaises(AssertionError):
                release.Release.preflight(r)
        self.assertTrue(r.running)
        self.assertFalse(r.loaded)

    def test_known_additive_migration_and_tampering(self):
        before = '001_initial.sql|old\n'
        added = release.MIGRATION + '|' + release.MIGRATION_CHECKSUM + '\n'
        release.check_migrations(before, before)
        release.check_migrations(before, before + added, require_new=True)
        release.check_migrations(before + added, before + added, require_new=True)
        for after in (before + added.replace(release.MIGRATION_CHECKSUM, 'bad'),
                      before.replace('old', 'changed') + added, added):
            with self.assertRaises(AssertionError):
                release.check_migrations(before, after)
        with self.assertRaises(AssertionError):
            release.check_migrations(before, before, require_new=True)

    def test_mode_backup_sql_and_cache_invalidation(self):
        r = self.r
        bk = r.deploy / 'backups' / 'test'
        bk.mkdir(parents=True)
        statements, commands = [], []
        row = {'id': 12, release.MODE_KEYS[0]: 'adaptive', release.MODE_KEYS[1]: 'off'}
        def sql(query):
            statements.append(query)
            return json.dumps(row) + '\n' if query.startswith('SELECT') else ''
        def dc(*args, data=None):
            commands.append(args)
            return '2\n'
        with patch.object(r, 'sql', sql), patch.object(r, 'dc', dc):
            release.Release.normalize_modes(r, bk)
        self.assertEqual(json.loads((bk / 'adaptive-modes.before-rollback.json').read_text()), [row])
        self.assertIn('BEGIN;', statements[1])
        self.assertIn('scheduler_outbox', statements[1])
        self.assertIn('passthrough', statements[1])
        self.assertNotIn('credentials', statements[0])
        self.assertEqual(commands[0][-3:], ('DEL', 'sched:acc:12', 'sched:meta:12'))
        with patch.object(r, 'sql', lambda q: ''), patch.object(r, 'dc', dc):
            release.Release.normalize_modes(r, bk)
        self.assertEqual(len(commands), 2)  # 重试仍清理首次备份账号缓存

    def test_existing_positive_env_preserved(self):
        self.r.env.write_text('SYNTHETIC_ONLY=1\n' + release.KEY + '=15728640\n')
        self.r.install()
        self.assertEqual(self.r.config()['environment'][release.KEY], '15728640')

    def test_compose_edit_list_map_and_non_target_preserved(self):
        for block in (COMPOSE, COMPOSE.replace('      - AUTO_SETUP=true', '      AUTO_SETUP: "true"').replace('      - GATEWAY_OPENAI_WS_ENABLED=true', '      GATEWAY_OPENAI_WS_ENABLED: "true"')):
            edited = release.edit_compose(block, IMAGE, True)
            self.assertEqual(edited.count(release.KEY), 0)
            self.assertEqual(edited.split('  postgres:')[1], block.split('  postgres:')[1])
            self.assertEqual(release.edit_compose(edited, IMAGE, True), edited)

    def test_image_identity_accepts_classic_config_digest(self):
        # 同一实际归档的 OCI manifest digest 与 config digest。
        manifest_id = 'sha256:c334b923716bf4ea38da6f17430449522f427e91e05b7c48bc5d6d339e576133'
        config_id = 'sha256:6f4e80a3ce0fac6e49175b1c86a60346bd006db1553cb77505ec443fd69b3d70'
        self.r.manifest.update(image_id=manifest_id, image_config_digest=config_id)
        def docker(*args, data=None):
            if args[:2] == ('image', 'inspect'):
                return json.dumps([dict(Id=config_id, Os='linux', Architecture='amd64')])
            if args[0] == 'run':
                return 'Sub2API ' + NEW
            raise AssertionError(args)
        with patch.object(self.r, 'd', docker):
            self.assertEqual(release.Release.check_image(self.r, IMAGE, NEW, manifest_id), config_id)

    def test_image_identity_keeps_other_integrity_checks(self):
        manifest_id = 'sha256:' + 'a' * 64
        config_id = 'sha256:' + 'b' * 64
        self.r.manifest.update(image_id=manifest_id, image_config_digest=config_id)
        scenarios = (
            (IMAGE, manifest_id, 'amd64', NEW, True),
            (IMAGE, config_id, 'amd64', NEW, True),
            (IMAGE, 'sha256:' + 'c' * 64, 'amd64', NEW, False),
            (IMAGE, config_id, 'arm64', NEW, False),
            (IMAGE, config_id, 'amd64', 'wrong-commit', False),
            ('sub2api:rollback-before-ws-oom-test', config_id, 'amd64', NEW, False),
        )
        for image, actual_id, arch, actual_commit, accepted in scenarios:
            with self.subTest(image=image, id=actual_id, arch=arch, commit=actual_commit):
                def docker(*args, data=None):
                    if args[:2] == ('image', 'inspect'):
                        return json.dumps([dict(Id=actual_id, Os='linux', Architecture=arch)])
                    if args[0] == 'run':
                        return 'Sub2API ' + actual_commit
                    raise AssertionError(args)
                with patch.object(self.r, 'd', docker):
                    if accepted:
                        self.assertEqual(release.Release.check_image(self.r, image, NEW, manifest_id), actual_id)
                    else:
                        with self.assertRaises(AssertionError):
                            release.Release.check_image(self.r, image, NEW, manifest_id)


if __name__ == '__main__':
    unittest.main(verbosity=2)
