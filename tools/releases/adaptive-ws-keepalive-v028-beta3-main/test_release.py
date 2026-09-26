import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import release
import assemble_package as build_package


class DualLegReleaseTests(unittest.TestCase):
    def test_complete_beta_package_passes_real_verification(self):
        with tempfile.TemporaryDirectory() as directory:
            here = Path(directory)
            for name in ('deploy.sh', 'rollback.sh', 'release.py', 'README.md', release.ADDITIVE_MIGRATION, 'image.tar.gz'):
                (here / name).write_text('synthetic\n')
            for name in ('deploy.sh', 'rollback.sh'):
                (here / name).chmod(0o755)
            manifest = dict(target='ac.thinkerlu.top', rollback_commit=release.BASE_COMMIT,
                            rollback_image=release.BASE_IMAGE, rollback_image_id=release.BASE_ID,
                            rollback_image_config_digest=release.BASE_CONFIG_ID,
                            rollback_binary_sha256=release.BASE_BINARY_SHA256,
                            release_branch='codex/release-sub2api', commit='a' * 40,
                            version=build_package.VERSION,
                            image='sub2api:' + build_package.VERSION + '-aaaaaaaaa',
                            archive='image.tar.gz', archive_sha256=release.digest(here / 'image.tar.gz'),
                            additive_migrations={release.ADDITIVE_MIGRATION: release.hashlib.sha256(b'synthetic').hexdigest()}, database_migrations_changed=True)
            (here / 'manifest.json').write_text(json.dumps(manifest))
            (here / 'SHA256SUMS').write_text(''.join(release.digest(file) + '  ' + file.name + '\n' for file in sorted(here.iterdir())))
            with patch.object(release, 'HERE', here):
                self.assertEqual(release.verify_package(manifest), here / 'image.tar.gz')
                for field, value in (('version', 'v0.2.10-dual-leg-thinking-ws'),
                                     ('image', 'sub2api:v0.2.10-dual-leg-thinking-ws-aaaaaaaaa'),
                                     ('image', 'sub2api:' + build_package.VERSION + '-bbbbbbbbb')):
                    with self.subTest(field=field, value=value), self.assertRaises(AssertionError):
                        release.verify_package({**manifest, field: value})

    def test_custom_version_preserves_upstream_version(self):
        self.assertEqual(build_package.UPSTREAM_TAG, 'v0.2.8')
        self.assertEqual(build_package.VERSION, 'v0.2.8-beta3-adaptive-ws-keepalive')
        self.assertRegex(build_package.VERSION, r'^' + build_package.UPSTREAM_TAG.replace('.', r'\.') + r'-beta[1-9][0-9]*-[a-z0-9-]+$')
        self.assertEqual(release.BASE_IMAGE, 'sub2api:v0.2.8-request-traffic-monitoring-7a3472327')

    def test_atomic_configuration_write_preserves_permissions_and_owner(self):
        with tempfile.TemporaryDirectory() as directory:
            file = Path(directory) / '.env'
            file.write_text('SYNTHETIC=before\n')
            file.chmod(0o600)
            before = file.stat()
            release.atomic(file, 'SYNTHETIC=after\n')
            self.assertEqual(file.read_text(), 'SYNTHETIC=after\n')
            self.assertEqual(file.stat().st_mode & 0o777, 0o600)
            self.assertEqual((file.stat().st_uid, file.stat().st_gid), (before.st_uid, before.st_gid))

    def test_main_uses_one_compose_file(self):
        with patch.object(release, 'run', return_value='') as run:
            release.compose('config', '--quiet')
        args = run.call_args.args[0]
        self.assertEqual(args.count('-f'), 1)
        self.assertNotIn('docker-compose.host-access.yml', ' '.join(args))

    def test_switches_only_sub2api_image_and_preserves_overlay(self):
        original = ('services:\n'
                    '  postgres:\n    image: postgres:18-alpine\n'
                    '  sub2api:\n    image: ' + release.BASE_IMAGE + '\n'
                    '    environment:\n      SERVER_PORT: 8080\n'
                    '  redis:\n    image: redis:8-alpine\n')
        updated = release.switched_compose(original, release.BASE_IMAGE, 'sub2api:v0.2.8-beta3-adaptive-ws-keepalive-123456789')
        self.assertEqual(updated.count('    image: sub2api:v0.2.8-beta3-adaptive-ws-keepalive-123456789'), 1)
        self.assertIn('    image: postgres:18-alpine', updated)
        self.assertIn('    image: redis:8-alpine', updated)
        self.assertIn('SERVER_PORT: 8080', updated)
        with self.assertRaises(AssertionError):
            release.switched_compose(updated, release.BASE_IMAGE, 'another')

    def test_rejects_wrong_target_before_reading_archive(self):
        with tempfile.TemporaryDirectory() as directory:
            here = Path(directory)
            (here / 'SHA256SUMS').write_text('')
            with patch.object(release, 'HERE', here):
                with self.assertRaises(AssertionError):
                    release.verify_package({
                        'archive': 'missing.tar.gz', 'archive_sha256': '', 'target': 'cc.thkl.site',
                        'rollback_commit': release.BASE_COMMIT, 'rollback_image': release.BASE_IMAGE,
                        'rollback_image_id': release.BASE_ID, 'release_branch': 'codex/release-sub2api',
                        'commit': 'a' * 40, 'image': 'sub2api:v0.2.8-beta3-adaptive-ws-keepalive-aaaaaaaaa'
                    })

    def test_rejects_nonbaseline_container_before_loading_image(self):
        with tempfile.TemporaryDirectory() as directory:
            deploy = Path(directory)
            (deploy / '.env').write_text('CONFIG=hidden\n')
            (deploy / 'docker-compose.host-access.yml').write_text('services: {}\n')
            with patch.object(release, 'DEPLOY', deploy), \
                 patch.object(release, 'verify_package', return_value=deploy / 'archive.tar.gz'), \
                 patch.object(release, 'compose', return_value=''), \
                 patch.object(release, 'current_container', return_value={'Config': {'Image': 'wrong-image'}}), \
                 patch.object(release, 'docker') as docker:
                with self.assertRaises(AssertionError):
                    release.check_preflight({})
                docker.assert_not_called()

    def test_refuses_rollback_after_migration_change(self):
        with tempfile.TemporaryDirectory() as directory:
            backup = Path(directory)
            (backup / 'backup.json').write_text(json.dumps({
                'new_commit': 'a' * 40, 'old_commit': release.BASE_COMMIT,
                'old_image_sha256': 'digest'}))
            (backup / 'old-image.tar').write_bytes(b'image')
            (backup / 'migrations.before.txt').write_text('241_usage_log_traffic_bytes.sql|before\n')
            with patch.object(release, 'digest', return_value='digest'), \
                 patch.object(release, 'docker'), \
                 patch.object(release, 'image_id', return_value=release.BASE_ID), \
                 patch.object(release, 'migrations', return_value='242_unexpected.sql|after\n'), \
                 patch.object(release, 'atomic') as atomic:
                with self.assertRaises(AssertionError):
                    release.restore(backup, {'commit': 'a' * 40})
                atomic.assert_not_called()

    def test_allows_only_242_and_preserves_existing_migrations(self):
        before = '241_usage_log_traffic_bytes.sql|' + 'a' * 64 + '\n'
        manifest = {'additive_migrations': {release.ADDITIVE_MIGRATION: 'b' * 64}}
        after = before + release.ADDITIVE_MIGRATION + '|' + 'b' * 64 + '\n'
        release.compatible_migrations(before, before, manifest)
        release.compatible_migrations(before, after, manifest)
        release.compatible_migrations(after, after, manifest)
        for invalid in (after.replace('a' * 64, 'c' * 64), after + '243_other.sql|' + 'd' * 64 + '\n', ''):
            with self.assertRaises(AssertionError):
                release.compatible_migrations(before, invalid, manifest)

    def test_main_health_uses_container_port_and_main_domain(self):
        with patch.object(release, 'current_container', return_value={'State': {'Health': {'Status': 'healthy'}}}), \
             patch.object(release, 'version', return_value=release.BASE_COMMIT), \
             patch.object(release, 'compose', side_effect=[release.BASE_BINARY_SHA256 + ' /app/sub2api', '']) as compose, \
             patch.object(release, 'run') as run:
            release.healthy(release.BASE_COMMIT)
        self.assertIn('http://127.0.0.1:8080/health', compose.call_args.args)
        self.assertIn('https://subapi.thinkerlu.top/health', run.call_args.args[0])

    def test_rollback_stops_app_then_waits_for_recovery_tombstones_without_deleting_them(self):
        calls = []
        scans = iter(['pending', 'replay', '', ''])
        def compose(*args, **kwargs):
            calls.append(args)
            if args[0] == 'config':
                return json.dumps({'services': {'sub2api': {'environment': {'REDIS_DB': '0'}}}})
            if '--scan' in args:
                return next(scans)
            return ''
        with patch.object(release, 'compose', side_effect=compose), patch.object(release.time, 'sleep') as sleep:
            release.drain_recovery_window()
        self.assertEqual(calls[1], ('stop', '-t', '30', 'sub2api'))
        sleep.assert_called_once_with(2)
        self.assertFalse(any('DEL' in call or 'FLUSHDB' in call for call in calls))

    def test_package_rejects_missing_required_files_even_with_valid_archive_hash(self):
        with tempfile.TemporaryDirectory() as directory:
            here = Path(directory)
            (here / 'SHA256SUMS').write_text('')
            manifest = {'target': 'ac.thinkerlu.top', 'rollback_commit': release.BASE_COMMIT,
                        'rollback_image': release.BASE_IMAGE, 'rollback_image_id': release.BASE_ID,
                        'rollback_image_config_digest': release.BASE_CONFIG_ID,
                        'rollback_binary_sha256': release.BASE_BINARY_SHA256,
                        'release_branch': 'codex/release-sub2api', 'commit': 'a' * 40,
                        'version': build_package.VERSION,
                        'image': 'sub2api:v0.2.8-beta3-adaptive-ws-keepalive-aaaaaaaaa', 'archive': 'image.tar.gz'}
            with patch.object(release, 'HERE', here), self.assertRaises(AssertionError):
                release.verify_package(manifest)


if __name__ == '__main__':
    unittest.main()
