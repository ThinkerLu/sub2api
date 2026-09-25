import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import release


class TrafficTrendReleaseTests(unittest.TestCase):
    def test_switches_only_sub2api_image_and_preserves_overlay(self):
        original = ('services:\n'
                    '  postgres:\n    image: postgres:18-alpine\n'
                    '  sub2api:\n    image: ' + release.BASE_IMAGE + '\n'
                    '    environment:\n      SERVER_PORT: 8080\n'
                    '  redis:\n    image: redis:8-alpine\n')
        updated = release.switched_compose(original, release.BASE_IMAGE, 'sub2api:v0.2.9-traffic-trend-dashboard-123456789')
        self.assertEqual(updated.count('    image: sub2api:v0.2.9-traffic-trend-dashboard-123456789'), 1)
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
                        'archive': 'missing.tar.gz', 'archive_sha256': '', 'target': 'ac.thinkerlu.top',
                        'rollback_commit': release.BASE_COMMIT, 'rollback_image': release.BASE_IMAGE,
                        'rollback_image_id': release.BASE_ID, 'release_branch': 'codex/release-sub2api',
                        'commit': 'a' * 40, 'image': 'sub2api:v0.2.9-traffic-trend-dashboard-aaaaaaaaa'
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


if __name__ == '__main__':
    unittest.main()
