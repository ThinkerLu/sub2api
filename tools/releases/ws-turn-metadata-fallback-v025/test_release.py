#!/usr/bin/env python3
"""Local release-script tests; no production access or credentials."""
import hashlib
from pathlib import Path
import json
from pathlib import Path
import tempfile
import unittest
from types import SimpleNamespace
from unittest.mock import patch

import release


NEW = 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb'
IMAGE = 'sub2api:v0.2.5-ws-turn-metadata-fallback-bbbbbbbbb'
COMPOSE = '''services:
  sub2api:
    image: sub2api:v0.2.5-ws-preflight-fallback-17866c78b
    environment:
      - AUTO_SETUP=true
    volumes:
      - ./data:/app/data
  postgres:
    image: postgres:18-alpine
    environment:
      - POSTGRES_USER=synthetic
'''


class ReleaseHelpersTests(unittest.TestCase):
    def test_run_combined_includes_stderr_version_output(self):
        completed = SimpleNamespace(returncode=0, stdout='Sub2API commit abc123\n')
        with patch.object(release.subprocess, 'run', return_value=completed) as run_mock:
            self.assertEqual(release.run_combined(['docker', 'run']), 'Sub2API commit abc123\n')
            self.assertIs(run_mock.call_args.kwargs['stderr'], release.subprocess.STDOUT)

    def test_current_version_uses_combined_container_output(self):
        completed = SimpleNamespace(returncode=0, stdout='Sub2API commit abc123\n')
        release_obj = object.__new__(release.Release)
        release_obj.docker = ['docker']
        release_obj.compose = Path('/synthetic/docker-compose.yml')
        with patch.object(release.subprocess, 'run', return_value=completed) as run_mock:
            self.assertEqual(release_obj.current_version(), 'Sub2API commit abc123\n')
            args = run_mock.call_args.args[0]
            self.assertEqual(args[:4], ['docker', 'compose', '-f', str(release_obj.compose)])
            self.assertEqual(args[4:7], ['exec', '-T', 'sub2api'])
            self.assertIs(run_mock.call_args.kwargs['stderr'], release.subprocess.STDOUT)

    def test_known_additive_migrations_match_runtime_checksum(self):
        migrations = release.HERE.parents[2] / 'backend' / 'migrations'
        if not migrations.is_dir():
            self.skipTest('source migrations are not present in the standalone release directory')
        for name, expected in release.KNOWN_ADDITIVE_MIGRATIONS.items():
            actual = hashlib.sha256((migrations / name).read_text().strip().encode()).hexdigest()
            self.assertEqual(actual, expected, name)

    def test_upsert_env_preserves_comments_and_updates_exact_keys(self):
        text = '# keep\nOTHER=1\nMODEL_REGISTRY_ENABLED=false\n'
        updated = release.upsert_env(text, release.REGISTRY_ENV)
        self.assertIn('# keep\n', updated)
        self.assertIn('OTHER=1\n', updated)
        self.assertIn('MODEL_REGISTRY_ENABLED=true\n', updated)
        self.assertEqual(updated.count('MODEL_REGISTRY_FILE='), 1)
        self.assertIn('MODEL_REGISTRY_FILE=/app/model-registry/model-registry.yaml\n', updated)

    def test_edit_compose_adds_read_only_registry_mount_once(self):
        edited = release.edit_compose(COMPOSE, IMAGE)
        self.assertIn('    image: ' + IMAGE + '\n', edited)
        self.assertEqual(edited.count(release.REGISTRY_MOUNT), 1)
        for key, value in release.REGISTRY_COMPOSE_ENV.items():
            self.assertIn(f'      {key}: "{value}"\n', edited)
        self.assertEqual(edited.split('  postgres:')[1], COMPOSE.split('  postgres:')[1])
        self.assertEqual(release.edit_compose(edited, IMAGE), edited)

    def test_edit_compose_can_leave_old_compose_unmounted_for_rollback(self):
        edited = release.edit_compose(COMPOSE, IMAGE, registry=False)
        self.assertNotIn(release.REGISTRY_MOUNT, edited)
        self.assertIn('image: ' + IMAGE, edited)


class FakeRelease(release.Release):
    def __init__(self, deploy):
        self.deploy = Path(deploy).resolve()
        self.image = IMAGE
        self.commit = NEW
        self.archive = self.deploy / 'image.tar.gz'
        self.compose = self.deploy / 'docker-compose.yml'
        self.env = self.deploy / '.env'
        self.current = release.OLD_COMMIT
        self.running = True
        self.baseline = '001_initial.sql|synthetic-checksum\n'
        self.calls = []
        self.manifest = {'archive_sha256': hashlib.sha256(self.archive.read_bytes()).hexdigest(), 'image_id': 'new-id'}

    def preflight(self):
        pass

    def current_version(self):
        return 'Sub2API ' + self.current

    def config(self):
        is_new = self.current == NEW
        compose_text = self.compose.read_text()
        environment = release.REGISTRY_ENV if is_new else {}
        if '${MODEL_REGISTRY_ENABLED:-true}' in compose_text:
            environment = {key: release.REGISTRY_COMPOSE_ENV[key].split(':-', 1)[1].rstrip('}') for key in release.REGISTRY_ENV}
        return {
            'image': IMAGE if is_new else release.OLD_IMAGE,
            'environment': environment,
            'volumes': [{'target': release.REGISTRY_CONTAINER_DIR, 'read_only': True}] if is_new else [],
        }

    def migrations(self):
        if self.current == NEW and release.REQUIRED_MIGRATIONS[0] not in self.baseline:
            for name in release.REQUIRED_MIGRATIONS:
                self.baseline += name + '|' + release.KNOWN_ADDITIVE_MIGRATIONS[name] + '\n'
        return self.baseline

    def space(self, _mib):
        pass

    def healthy(self, commit):
        assert self.current == commit and self.running

    def check_image(self, _image, _commit, _expected_id=None):
        return 'snapshot-id'

    def d(self, *args, data=None):
        self.calls.append(('docker', args))
        if args == ('ps', '-aq'):
            return 'cid'
        if args[0] == 'inspect':
            return json.dumps([{'Image': 'current-id', 'Mounts': [{'Destination': '/app/data'}]}])
        if args[0] == 'commit':
            return 'snapshot-id'
        if args[0] == 'load':
            return ''
        raise AssertionError(args)

    def dc(self, *args, data=None):
        self.calls.append(('compose', args))
        if args == ('ps', '-q', 'sub2api'):
            return 'cid' if self.running else ''
        if args[0] == 'stop':
            self.running = False
        elif args[0] == 'start':
            self.running = True
        elif args[0] == 'up':
            self.running = True
            self.current = NEW if 'image: ' + IMAGE in self.compose.read_text() else release.OLD_COMMIT
        elif args[0] == 'kill':
            assert args[:3] == ('kill', '-s', 'HUP')
        return ''

    def verify_registry_runtime(self):
        assert release.REGISTRY_MOUNT.strip() in self.compose.read_text()


class ReleaseFlowTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        root = Path(self.tmp.name)
        self.patch_home = patch.object(release, 'HERE', root)
        self.patch_home.start()
        self.addCleanup(self.patch_home.stop)
        (root / 'image.tar.gz').write_bytes(b'synthetic image')
        (root / 'model-registry.example.yaml').write_text('version: 1\nmodels: {}\n')
        (root / 'docker-compose.yml').write_text(COMPOSE)
        (root / '.env').write_text('SYNTHETIC_ONLY=1\n')

    def test_deploy_and_rollback_preserve_external_registry(self):
        registry_dir = Path(self.tmp.name) / release.REGISTRY_DIR
        registry_dir.mkdir()
        registry = registry_dir / release.REGISTRY_FILE
        registry.write_text('operator-owned: true\n')
        r = FakeRelease(self.tmp.name)

        r.install()
        self.assertEqual(registry.read_text(), 'operator-owned: true\n')
        self.assertIn(release.REGISTRY_MOUNT.strip(), r.compose.read_text())
        self.assertIn('MODEL_REGISTRY_ENABLED=true', r.env.read_text())

        r.rollback()
        self.assertEqual(registry.read_text(), 'operator-owned: true\n')
        self.assertIn(release.REGISTRY_MOUNT.strip(), r.compose.read_text())

    def test_reload_sends_hup_without_restart(self):
        r = FakeRelease(self.tmp.name)
        r.compose.write_text(release.edit_compose(COMPOSE, IMAGE))
        (Path(self.tmp.name) / release.REGISTRY_DIR).mkdir()
        (Path(self.tmp.name) / release.REGISTRY_DIR / release.REGISTRY_FILE).write_text('version: 1\nmodels: {}\n')
        r.reload()
        self.assertIn(('compose', ('kill', '-s', 'HUP', 'sub2api')), r.calls)


if __name__ == '__main__':
    unittest.main()
