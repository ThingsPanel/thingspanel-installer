"""Persistent ThingsVis auth identity; private filesystem, no services or DB."""
import importlib.util
import os
from pathlib import Path
import tempfile
import subprocess
import json
import unittest
from unittest import mock
from test_update_transaction import BASE, CONFIG, payload_download

REPO = Path(__file__).resolve().parents[1]
SPEC = importlib.util.spec_from_file_location('tp_prepare', REPO / 'config/native/prepare.py')
prepare = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(prepare)
KEY = 'TP_THINGSVIS_AUTH_SECRET'


class ThingsVisAuthSecretTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='tp-auth-secret-')
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.config = self.root / 'config/native'
        self.config.mkdir(parents=True)
        (self.config / 'internal.crt').write_text('fixture certificate; no OpenSSL invocation')
        for patch in (mock.patch.object(prepare, 'ROOT', self.root),
                      mock.patch.object(prepare, 'CONFIG', self.config),
                      mock.patch.object(prepare.os, 'geteuid', return_value=1000, create=True),
                      mock.patch.dict(os.environ, {}, clear=False)):
            patch.start()
            self.addCleanup(patch.stop)
        for key in (KEY, 'TP_THINGSVIS_IMAGE', 'TP_THINGSVIS_LOCAL_IMAGE_ID', 'TP_THINGSVIS_AUTH_POLICY_TARGET', 'TP_MARKETINSTALL_PROVIDER_TOKEN', 'TP_INSTALLER_ACTIVE_OPERATION',
                    'TP_MARKETINSTALL_THINGSVIS_IDENTITY_TOKEN', 'TP_MARKETINSTALL_THINGSVIS_TOKEN'):
            os.environ.pop(key, None)

    def test_first_install_persists_a_strong_unique_secret_and_upgrade_reuses_it(self):
        first = prepare.configure()[KEY]
        first_env = (self.root / '.env').read_bytes()
        self.assertEqual(len(first), 64)
        self.assertRegex(first, r'^[0-9a-f]{64}$')
        self.assertEqual(prepare.configure()[KEY], first)
        self.assertEqual((self.root / '.env').read_bytes(), first_env)
        self.assertEqual(first_env.count((KEY + '=').encode()), 1)
        self.assertNotEqual(prepare.deployment_secret({}, KEY), first)
        self.assertNotIn('TP_THINGSVIS_AUTH_POLICY=', first_env.decode())

    def test_existing_secret_is_preserved(self):
        original = 'existing-deployment-secret-' + 'a' * 40
        (self.root / '.env').write_text(KEY + '=' + original + '\n')
        self.assertEqual(prepare.configure()[KEY], original)
        self.assertEqual(prepare.configure()[KEY], original)
        self.assertEqual((self.root / '.env').read_text().count(KEY + '='), 1)

    def test_explicit_environment_seed_is_persisted_then_required_to_match(self):
        seed = 'reviewed-first-install-secret-' + 'b' * 40
        os.environ[KEY] = seed
        self.assertEqual(prepare.configure()[KEY], seed)
        before = (self.root / '.env').read_bytes()
        os.environ[KEY] = 'conflicting-secret-' + 'c' * 40
        with self.assertRaisesRegex(RuntimeError, 'conflicts'):
            prepare.configure()
        self.assertEqual((self.root / '.env').read_bytes(), before)

    def test_invalid_existing_secret_fails_before_any_identity_is_written(self):
        for value in ('', 'short', 'your-super-secret-auth-key'):
            with self.subTest(value=value):
                original = (KEY + '=' + value + '\n').encode()
                (self.root / '.env').write_bytes(original)
                with self.assertRaisesRegex(RuntimeError, 'at least 32'):
                    prepare.configure()
                self.assertEqual((self.root / '.env').read_bytes(), original)
                self.assertFalse((self.config / 'broker/owner-tokens.json').exists())

    def staged_upgrade(self, old_secret):
        import copy
        config = copy.deepcopy(CONFIG)
        config['services']['thingsvis-server']['environment']['AUTH_SECRET'] = old_secret
        files = prepare.update_files()
        patch = mock.patch.object(files, 'render_with_fallback', return_value=config)
        patch.start()
        self.addCleanup(patch.stop)
        (self.root / 'docker-compose.yml').write_text('old compose fixture')
        transaction = files.Transaction(self.root, downloader=payload_download)
        data = transaction.prepare(BASE, '')
        transaction.apply(data)
        data['phase'] = 'activation_started'
        transaction.save(data)
        os.environ['TP_INSTALLER_ACTIVE_OPERATION'] = data['operation']
        return transaction, data

    def test_secure_upgrade_rotates_old_sso_identity_once_and_journals_env(self):
        original = 'custom-inline-secret-' + 'd' * 48
        previous_env = (KEY + '=' + original + '\n').encode()
        (self.root / '.env').write_bytes(previous_env)
        transaction, data = self.staged_upgrade(original)
        os.environ['TP_THINGSVIS_IMAGE'] = 'local/thingsvis-reviewed:fixture'
        os.environ['TP_THINGSVIS_AUTH_POLICY_TARGET'] = 'verified-sso-v1'
        first = prepare.configure()[KEY]
        self.assertNotEqual(first, original)
        self.assertEqual(prepare.configure()[KEY], first)
        text = (self.root / '.env').read_text()
        self.assertIn('TP_THINGSVIS_AUTH_POLICY=verified-sso-v1\n', text)
        self.assertEqual(text.count('TP_THINGSVIS_AUTH_POLICY='), 1)
        self.assertEqual((transaction.state / data['operation'] / 'backup/.env').read_bytes(), previous_env)
        entry = transaction.load()['activation_files']['.env']
        self.assertTrue(entry['applied'])
        self.assertTrue(prepare.update_files().same(entry['new'], prepare.update_files().file_info(self.root / '.env')))

    def test_secure_policy_cannot_be_claimed_for_default_legacy_image(self):
        os.environ['TP_THINGSVIS_AUTH_POLICY_TARGET'] = 'verified-sso-v1'
        for image in ('', 'registry/thingsvis-server:v1.0.23'):
            with self.subTest(image=image):
                os.environ['TP_THINGSVIS_IMAGE'] = image
                with self.assertRaisesRegex(RuntimeError, 'reviewed secure'):
                    prepare.configure()
                self.assertFalse((self.root / '.env').exists())

    def test_secured_identity_is_reused_and_policy_downgrade_is_rejected(self):
        os.environ['TP_THINGSVIS_IMAGE'] = 'local/thingsvis-reviewed:fixture'
        os.environ['TP_THINGSVIS_AUTH_POLICY_TARGET'] = 'verified-sso-v1'
        first = prepare.configure()[KEY]
        self.assertEqual(prepare.configure()[KEY], first)
        before = (self.root / '.env').read_bytes()
        os.environ['TP_THINGSVIS_AUTH_POLICY_TARGET'] = ''
        with self.assertRaisesRegex(RuntimeError, 'downgrade'):
            prepare.configure()
        self.assertEqual((self.root / '.env').read_bytes(), before)

    def test_policy_rotation_rejects_exported_secret_before_any_write(self):
        os.environ['TP_THINGSVIS_IMAGE'] = 'local/thingsvis-reviewed:fixture'
        os.environ['TP_THINGSVIS_AUTH_POLICY_TARGET'] = 'verified-sso-v1'
        os.environ[KEY] = 'old-exported-secret-' + 'a' * 48
        with self.assertRaisesRegex(RuntimeError, 'would override'):
            prepare.configure()
        self.assertFalse((self.root / '.env').exists())
        self.assertFalse((self.config / 'broker').exists())

    def test_existing_quoted_secret_keeps_its_literal_dotenv_encoding(self):
        value = 'literal-dollar-$not-an-env-reference-' + 'b' * 32
        saved = "'" + value + "'"
        (self.root / '.env').write_text(KEY + '=' + saved + '\n')
        os.environ[KEY] = value
        self.assertEqual(prepare.configure()[KEY], saved)
        before = (self.root / '.env').read_bytes()
        self.assertIn((KEY + '=' + saved + '\n').encode(), before)
        self.assertEqual(prepare.configure()[KEY], saved)
        self.assertEqual((self.root / '.env').read_bytes(), before)

    def test_integration_tokens_are_distinct_persistent_and_conflicts_fail_closed(self):
        keys = ('TP_MARKETINSTALL_PROVIDER_TOKEN', 'TP_MARKETINSTALL_THINGSVIS_IDENTITY_TOKEN', 'TP_MARKETINSTALL_THINGSVIS_TOKEN')
        first = prepare.configure()
        self.assertEqual(len({first[key] for key in keys}), 3)
        self.assertTrue(all(len(first[key]) == 64 for key in keys))
        second = prepare.configure()
        self.assertEqual([first[key] for key in keys], [second[key] for key in keys])
        before = (self.root / '.env').read_bytes()
        os.environ[keys[0]] = 'different-provider-token-' + 'f' * 40
        with self.assertRaisesRegex(RuntimeError, 'conflicts'):
            prepare.configure()
        self.assertEqual((self.root / '.env').read_bytes(), before)

    def test_legacy_broker_identity_and_services_are_preserved_in_minimal_directory(self):
        import json
        token = 'legacy-broker-token-' + 'a' * 48
        services = [{'username': 'existing', 'allowed_device_ids': ['fixture-device']}]
        old_token = self.config / 'plugin-token'
        old_services = self.config / 'services.json'
        old_owners = self.config / 'owner-tokens.json'
        owners = [{'token': token, 'broker_cell': 'community', 'broker_instance': 'community-mosquitto'},
                  {'token': 'other-preexisting-token-' + 'b' * 40, 'broker_cell': 'other', 'broker_instance': 'existing'}]
        old_token.write_text(token)
        old_services.write_text(json.dumps(services))
        old_owners.write_text(json.dumps(owners))
        old_token.chmod(0o440)
        old_services.chmod(0o440)
        old_owners.chmod(0o440)
        before = (old_token.read_bytes(), old_services.read_bytes(), old_owners.read_bytes())
        prepare.configure()
        self.assertEqual((self.config / 'broker/plugin-token').read_text(), token)
        self.assertEqual(json.loads((self.config / 'broker/services.json').read_text()), services)
        self.assertEqual(json.loads((self.config / 'broker/owner-tokens.json').read_text()), owners)
        self.assertEqual({p.name for p in (self.config / 'broker').iterdir()},
                         {'plugin-token', 'owner-tokens.json', 'services.json'})
        self.assertEqual((old_token.read_bytes(), old_services.read_bytes(), old_owners.read_bytes()), before)
        prepare.configure()
        self.assertEqual((old_token.read_bytes(), old_services.read_bytes(), old_owners.read_bytes()), before)

    def test_originally_absent_broker_files_and_directory_are_journaled(self):
        transaction, _ = self.staged_upgrade('old-inline-secret-' + 'b' * 48)
        self.assertFalse((self.config / 'broker').exists())
        prepare.configure()
        data = transaction.load()
        self.assertEqual(data['phase'], 'activation_started')
        self.assertIn('config/native/broker', data['activation_dirs'])
        for name in ('plugin-token', 'owner-tokens.json', 'services.json'):
            entry = data['activation_files']['config/native/broker/' + name]
            self.assertIsNone(entry['old'])
            self.assertTrue(entry['applied'])
            self.assertTrue(prepare.update_files().same(entry['new'], prepare.update_files().file_info(self.config / 'broker' / name)))

    def test_standalone_prepare_cannot_resume_unknown_activation(self):
        _, data = self.staged_upgrade('old-secret-' + 'c' * 48)
        os.environ.pop('TP_INSTALLER_ACTIVE_OPERATION')
        with self.assertRaisesRegex(RuntimeError, 'active installer operation'):
            prepare.configure()
        self.assertFalse((self.root / '.env').exists())
        self.assertFalse((self.config / 'broker').exists())
        os.environ['TP_INSTALLER_ACTIVE_OPERATION'] = data['operation']
        prepare.configure()
        before = (self.root / '.env').read_bytes()
        os.environ.pop('TP_INSTALLER_ACTIVE_OPERATION')
        with self.assertRaisesRegex(RuntimeError, 'active installer operation'):
            prepare.configure()
        self.assertEqual((self.root / '.env').read_bytes(), before)

    def test_explicit_completed_device_scope_refresh_remains_serialized_and_atomic(self):
        import json
        transaction, _ = self.staged_upgrade('old-secret-' + 'd' * 48)
        prepare.configure()
        data = transaction.load()
        data['phase'] = 'complete'
        transaction.save(data)
        os.environ.pop('TP_INSTALLER_ACTIVE_OPERATION')
        services = [{'username': 'fixture', 'allowed_device_ids': ['new-device']}]
        prepare.save('services.json', services, True)
        self.assertEqual(json.loads((self.config / 'broker/services.json').read_text()), services)
        with prepare.update_files().update_lock(transaction.state):
            with self.assertRaisesRegex(RuntimeError, 'Another installer'):
                prepare.save('services.json', [{'different': 'scope'}], True)


class LocalImageSelectionTests(unittest.TestCase):
    def test_exact_local_image_identity_is_used_without_registry_pull(self):
        image = 'local/thingsvis-reviewed:fixture'
        image_id = 'sha256:' + 'a' * 64
        config = {'services': {'thingsvis-server': {'image': image}, 'backend': {'image': 'registry/backend:fixture'}}}
        with mock.patch.object(prepare, 'command', side_effect=[json.dumps(config), image_id]) as command, mock.patch.object(
                prepare.subprocess, 'run', return_value=subprocess.CompletedProcess([], 0)) as run:
            prepare.pull_images({'TP_THINGSVIS_IMAGE': image, 'TP_THINGSVIS_LOCAL_IMAGE_ID': image_id})
        self.assertEqual(command.call_args.args, ('docker', 'image', 'inspect', '--format', '{{.Id}}', image))
        self.assertEqual(run.call_count, 1)
        self.assertEqual(run.call_args.args[0], ['docker', 'pull', 'registry/backend:fixture'])

    def test_changed_local_image_identity_fails_before_any_pull(self):
        image = 'local/thingsvis-reviewed:fixture'
        config = {'services': {'thingsvis-server': {'image': image}, 'backend': {'image': 'registry/backend:fixture'}}}
        with mock.patch.object(prepare, 'command', side_effect=[json.dumps(config), 'sha256:' + 'b' * 64]), mock.patch.object(
                prepare.subprocess, 'run') as run:
            with self.assertRaisesRegex(RuntimeError, 'identity differs'):
                prepare.pull_images({'TP_THINGSVIS_IMAGE': image, 'TP_THINGSVIS_LOCAL_IMAGE_ID': 'sha256:' + 'a' * 64})
        run.assert_not_called()


if __name__ == '__main__':
    unittest.main()
