import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import release
import build_package


class DualLegReleaseTests(unittest.TestCase):
    def test_skip_database_backup_creates_no_dump_and_runs_no_command(self):
        with tempfile.TemporaryDirectory() as directory, patch.object(release, 'run') as run:
            backup = Path(directory)
            self.assertEqual(release.backup_database(backup, skip_database_backup=True),
                             {'database_backup': 'skipped_by_operator', 'database_sha256': None})
            run.assert_not_called()
            self.assertFalse((backup / 'database.dump').exists())

    def test_default_database_backup_records_digest_and_rejects_empty_dump(self):
        with tempfile.TemporaryDirectory() as directory:
            backup = Path(directory)
            def dump(args, *, output):
                self.assertIn('pg_dump', args[-1])
                output.write(b'synthetic dump')
            with patch.object(release, 'run', side_effect=dump):
                meta = release.backup_database(backup)
            self.assertEqual(meta['database_backup'], 'completed')
            self.assertEqual(meta['database_sha256'], release.digest(backup / 'database.dump'))
            with patch.object(release, 'run'), self.assertRaises(AssertionError):
                release.backup_database(backup)

    def test_preflight_reports_space_without_blocking_either_backup_mode(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / '.env').touch()
            archive = root / 'image.tar.gz'
            archive.write_bytes(b'image')
            state = {'Config': {'Image': release.BASE_IMAGE, 'Labels': {
                'com.docker.compose.project.config_files': str(root / 'docker-compose.yml')}},
                'Image': release.BASE_ID}
            def compose(*args):
                if args[-1] == 'PING':
                    return 'PONG'
                if 'pg_database_size' in args[-1]:
                    return str(1024 ** 3)
                return ''
            with patch.object(release, 'DEPLOY', root), \
                 patch.object(release, 'verify_package', return_value=archive), \
                 patch.object(release, 'current_container', return_value=state), \
                 patch.object(release, 'image_id', return_value=release.BASE_ID), \
                 patch.object(release, 'compose_image', return_value=release.BASE_IMAGE), \
                 patch.object(release, 'version', return_value=release.BASE_COMMIT), \
                 patch.object(release, 'healthy'), \
                 patch.object(release, 'migrations', return_value='baseline'), \
                 patch.object(release, 'compatible_migrations'), \
                 patch.object(release, 'compose', side_effect=compose) as commands, \
                 patch.object(release.shutil, 'disk_usage') as usage:
                usage.return_value.free = 1024 ** 3
                self.assertEqual(release.check_preflight({}), 'baseline')
                self.assertEqual(release.check_preflight({}, skip_database_backup=True), 'baseline')
                self.assertFalse(any('pg_database_size' in str(call) for call in commands.call_args_list))
                usage.return_value.free = 1
                with patch('builtins.print') as output:
                    self.assertEqual(release.check_preflight({}, skip_database_backup=True), 'baseline')
                    self.assertEqual(release.check_preflight({}), 'baseline')
                self.assertTrue(any('1 字节' in str(call) for call in output.call_args_list))

    def test_deploy_passes_skip_choice_to_preflight(self):
        with patch.object(release, 'check_preflight', side_effect=RuntimeError('stop')) as preflight:
            with self.assertRaises(RuntimeError):
                release.deploy({}, skip_database_backup=True)
            preflight.assert_called_once_with({}, skip_database_backup=True, skip_image_backup=False)

    def test_complete_beta_package_passes_real_verification(self):
        with tempfile.TemporaryDirectory() as directory:
            here = Path(directory)
            for name in ('deploy.sh', 'rollback.sh', 'release.py', 'README.md', 'image.tar.gz'):
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
                            additive_migrations={}, database_migrations_changed=False)
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
        self.assertEqual(build_package.UPSTREAM_TAG, 'v0.2.9')
        self.assertEqual(build_package.VERSION, 'v0.2.9-beta1-ws-standalone-named-output')
        self.assertRegex(build_package.VERSION, r'^' + build_package.UPSTREAM_TAG.replace('.', r'\.') + r'-beta[1-9][0-9]*-[a-z0-9-]+$')
        self.assertEqual(release.BASE_IMAGE, 'sub2api:v0.2.8-beta3-adaptive-ws-keepalive-b3a100e05')

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
        updated = release.switched_compose(original, release.BASE_IMAGE, 'sub2api:v0.2.9-beta1-ws-standalone-named-output-123456789')
        self.assertEqual(updated.count('    image: sub2api:v0.2.9-beta1-ws-standalone-named-output-123456789'), 1)
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
                        'commit': 'a' * 40, 'image': 'sub2api:v0.2.9-beta1-ws-standalone-named-output-aaaaaaaaa'
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
            (backup / release.IMAGE_BACKUP).write_bytes(b'image')
            (backup / 'migrations.before.txt').write_text('241_usage_log_traffic_bytes.sql|before\n')
            with patch.object(release, 'digest', return_value='digest'), \
                 patch.object(release, 'docker'), \
                 patch.object(release, 'run'), \
                 patch.object(release, 'image_id', return_value=release.BASE_ID), \
                 patch.object(release, 'migrations', return_value='242_unexpected.sql|after\n'), \
                 patch.object(release, 'atomic') as atomic:
                with self.assertRaises(AssertionError):
                    release.restore(backup, {'commit': 'a' * 40})
                atomic.assert_not_called()

    def test_cleanup_removes_only_incomplete_unpointed_backups(self):
        with tempfile.TemporaryDirectory() as directory:
            deploy = Path(directory)
            root = deploy / 'backups'
            root.mkdir()
            incomplete = root / (release.BACKUP_PREFIX + 'incomplete')
            incomplete.mkdir()
            (incomplete / 'database.dump').write_bytes(b'partial')
            complete = root / (release.BACKUP_PREFIX + 'complete')
            complete.mkdir()
            (complete / 'backup.json').write_text('{}')
            other = root / 'other-release'
            other.mkdir()
            pointed = root / (release.BACKUP_PREFIX + 'pointed')
            pointed.mkdir()
            pointer = deploy / '.pointer'
            pointer.write_text(str(pointed) + '\n')
            with patch.object(release, 'DEPLOY', deploy), patch.object(release, 'POINTER', pointer):
                self.assertEqual(release.cleanup_incomplete_backups(), [incomplete.name])
            self.assertFalse(incomplete.exists())
            self.assertTrue(complete.exists())
            self.assertTrue(other.exists())
            self.assertTrue(pointed.exists())

    def test_base_image_backup_uses_compressed_stream_and_verifies_archive(self):
        with tempfile.TemporaryDirectory() as directory:
            backup = Path(directory)
            def run(args, **kwargs):
                if args[0] == 'bash':
                    (backup / release.IMAGE_BACKUP).write_bytes(b'compressed-image')
                return ''
            with patch.object(release, 'run', side_effect=run) as mocked:
                archive = release.save_base_image(backup)
            self.assertEqual(archive.name, release.IMAGE_BACKUP)
            self.assertEqual(mocked.call_args_list[0].args[0][0], 'bash')
            self.assertIn('gzip -1', mocked.call_args_list[0].args[0][4])
            self.assertEqual(mocked.call_args_list[1].args[0][:2], ['gzip', '-t'])

    def test_skip_image_backup_records_local_tag_retention(self):
        with tempfile.TemporaryDirectory() as directory:
            backup = Path(directory)
            with patch.object(release, 'run') as run:
                self.assertIsNone(release.save_base_image(backup, skip_image_backup=True))
            run.assert_not_called()

    def test_restore_uses_local_old_image_without_archive(self):
        with tempfile.TemporaryDirectory() as directory:
            backup = Path(directory)
            (backup / 'backup.json').write_text(json.dumps({
                'new_commit': 'a' * 40,
                'old_commit': release.BASE_COMMIT,
                'old_image_backup': 'local-tag-retained',
                'old_image_sha256': None,
                'old_image_id': release.BASE_ID,
            }))
            (backup / 'migrations.before.txt').write_text('migrations')
            (backup / '.env').write_text('CONFIG=hidden\n')
            (backup / 'docker-compose.yml').write_text('services: {}\n')
            with patch.object(release, 'image_id', return_value=release.BASE_ID), \
                 patch.object(release, 'migrations', return_value='migrations'), \
                 patch.object(release, 'compatible_migrations'), \
                 patch.object(release, 'drain_recovery_window'), \
                 patch.object(release, 'atomic'), \
                 patch.object(release, 'compose'), \
                 patch.object(release, 'healthy'), \
                 patch.object(release, 'run') as run:
                release.restore(backup, {'commit': 'a' * 40})
            run.assert_not_called()

    def test_ensure_new_image_reuses_verified_local_image(self):
        manifest = {
            'image': 'sub2api:test',
            'image_id': release.BASE_ID,
            'image_config_digest': release.BASE_CONFIG_ID,
            'commit': release.BASE_COMMIT,
            'archive': 'image.tar.gz',
        }
        info = json.dumps([{'Id': release.BASE_ID, 'Os': 'linux', 'Architecture': 'amd64'}])
        with patch.object(release, 'docker', side_effect=[info, release.BASE_COMMIT]) as docker:
            release.ensure_new_image(manifest)
        self.assertEqual([call.args[:2] for call in docker.call_args_list], [('image', 'inspect'), ('run', '--rm')])

    def test_requires_identical_migrations(self):
        before = ('241_usage_log_traffic_bytes.sql|' + 'a' * 64 + '\n' +
                  '242_usage_log_ingress_transport.sql|' + 'b' * 64 + '\n')
        manifest = {'database_migrations_changed': False, 'additive_migrations': {}}
        release.compatible_migrations(before, before, manifest)
        for invalid in (before.replace('a' * 64, 'c' * 64), before + '243_other.sql|' + 'd' * 64 + '\n', ''):
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
                        'image': 'sub2api:v0.2.9-beta1-ws-standalone-named-output-aaaaaaaaa', 'archive': 'image.tar.gz'}
            with patch.object(release, 'HERE', here), self.assertRaises(AssertionError):
                release.verify_package(manifest)


if __name__ == '__main__':
    unittest.main()
