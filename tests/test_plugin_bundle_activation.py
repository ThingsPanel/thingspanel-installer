"""Signed plugin-bundle staging and recovery without Docker or network."""
import hashlib
import importlib.util
import json
from pathlib import Path
import shutil
import subprocess
import tempfile
import time
import unittest
from unittest import mock

REPO = Path(__file__).resolve().parents[1]
SPEC = importlib.util.spec_from_file_location('tp_bundle_activation_update', REPO / 'config/native/update.py')
update = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(update)


class BundleActivation(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='tp-bundle-activation-')
        self.addCleanup(self.temp.cleanup)
        self.base = Path(self.temp.name).resolve()
        self.root = self.base / 'stack'
        self.root.mkdir()
        self.key = self.base / 'test-key.pem'
        self.trust = self.base / 'test-public.pem'
        subprocess.run(['openssl', 'ecparam', '-name', 'prime256v1', '-genkey', '-noout', '-out', str(self.key)],
                       check=True, capture_output=True)
        subprocess.run(['openssl', 'pkey', '-in', str(self.key), '-pubout', '-out', str(self.trust)],
                       check=True, capture_output=True)
        self.agent, self.image = b'agent-binary-fixture', b'image-archive-fixture'
        entry = json.loads((REPO / 'plugins/homeassistant.json').read_text())
        entry.update(image_platform='linux/amd64', image_archive_sha256=hashlib.sha256(self.image).hexdigest())
        self.catalog = {'schema': 'thingspanel.plugin-catalog.v1', 'version': 'fixture',
                        'published_at': int(time.time()), 'expires_at': int(time.time()) + 3600,
                        'target_platform': 'linux/amd64', 'release_tag': 'plugin-v1.0.0',
                        'agent_sha256': hashlib.sha256(self.agent).hexdigest(), 'plugins': [entry]}

    def signed(self, catalog):
        raw = json.dumps(catalog, sort_keys=True).encode()
        sig = subprocess.run(['openssl', 'dgst', '-sha256', '-sign', str(self.key)], input=raw,
                             stdout=subprocess.PIPE, check=True).stdout
        return raw, sig

    def download_helper(self, url, path):
        relative = url.removeprefix('https://install.thingspanel.io/')
        path.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(REPO / relative, path)

    def fake_assets(self, module):
        raw, sig = self.signed(self.catalog)
        urls = {
            module.DEFAULT_PAGES_BASE_URL + '/plugins/catalog-linux-amd64.json': raw,
            module.DEFAULT_PAGES_BASE_URL + '/plugins/catalog-linux-amd64.json.sig': sig,
            module.DEFAULT_RELEASE_BASE_URL + '/plugin-v1.0.0/tp-agent-linux-amd64': self.agent,
            module.DEFAULT_RELEASE_BASE_URL + '/plugin-v1.0.0/ha-image-linux-amd64.tar': self.image,
        }
        def fetch(url, destination, maximum):
            data = urls[url]
            if len(data) > maximum:
                raise RuntimeError('fixture exceeded limit')
            Path(destination).write_bytes(data)
        module._download_stream = fetch
        return module

    def stage(self):
        real_loader = update.load_plugin_helpers
        def load(root):
            return self.fake_assets(real_loader(root))
        with mock.patch.object(update, 'download', side_effect=self.download_helper), \
                mock.patch.object(update, 'load_plugin_helpers', side_effect=load), \
                mock.patch.object(update, 'run_command', return_value=b'linux/x86_64'):
            return update.prepare_optional_plugin(self.root, str(self.trust), allow_download=True)

    def test_stage_commit_and_restore_without_network(self):
        bundle, trust, catalog, pending = self.stage()
        self.assertTrue(pending)
        self.assertEqual(catalog['target_platform'], 'linux/amd64')
        self.assertFalse((self.root / '.thingspanel-update/plugin-bundles/current.json').exists())
        self.assertEqual(update.pending_plugin_directory(self.root), bundle)
        update.record_active_plugin(self.root, bundle)
        update.clear_pending_plugin(self.root)
        with mock.patch.object(update, 'download', side_effect=AssertionError('must not download')), \
                mock.patch.object(update, 'run_command', return_value=b'linux/amd64'):
            restored = update.prepare_optional_plugin(self.root, str(self.trust), allow_download=False)
        self.assertEqual(restored[0], bundle)
        self.assertFalse(restored[3])
        self.assertEqual(restored[2], catalog)

    def test_active_bundle_tampering_and_pointer_escape_fail_closed(self):
        bundle, _, _, _ = self.stage()
        update.record_active_plugin(self.root, bundle)
        update.clear_pending_plugin(self.root)
        (bundle / 'bin/tp-agent').write_bytes(b'tampered')
        with mock.patch.object(update, 'run_command', return_value=b'linux/amd64'):
            self.assertIsNone(update.optional_plugin_for_activation(self.root, str(self.trust), allow_download=False))
        pointer = self.root / '.thingspanel-update/plugin-bundles/current.json'
        pointer.write_text(json.dumps({'directory': '../escape'}))
        with self.assertRaises(update.UpdateError):
            update.active_plugin_directory(self.root)

    def test_expired_active_catalog_can_restore_but_not_be_staged_again(self):
        bundle, _, _, _ = self.stage()
        self.catalog['expires_at'] = int(time.time()) - 1
        raw, sig = self.signed(self.catalog)
        (bundle / 'plugins/plugin-catalog.json').write_bytes(raw)
        (bundle / 'plugins/plugin-catalog.json.sig').write_bytes(sig)
        update.record_active_plugin(self.root, bundle)
        with mock.patch.object(update, 'run_command', return_value=b'linux/amd64'):
            restored = update.prepare_optional_plugin(self.root, str(self.trust), allow_download=False)
        self.assertEqual(restored[2]['expires_at'], self.catalog['expires_at'])

    def test_rejected_optional_bundle_does_not_start_agent_or_block_platform(self):
        operation = {'phase': 'activation_started', 'operation': 'a' * 32}
        calls = []
        def run(args, cwd, **kwargs):
            calls.append(args)
            return b''
        with mock.patch.object(update.Transaction, 'load', return_value=operation), \
                mock.patch.object(update, 'verify_plugin_agent_bundle', return_value=False), \
                mock.patch.object(update, 'prepare_optional_plugin', side_effect=update.UpdateError('bad signature')), \
                mock.patch.object(update, 'run_command', side_effect=run), \
                mock.patch.object(update, 'restart_services_with_replaced_mounts', return_value=[]):
            self.assertFalse(update.activate_native(self.root, str(self.trust), enable_plugin_bundle=True))
        self.assertFalse(any('agent_host.py' in ' '.join(args) or args[:2] == ['docker', 'load'] for args in calls))
        self.assertTrue(any(args[:3] == ['docker', 'compose', 'up'] for args in calls))

    def test_pointer_is_written_only_after_agent_starts(self):
        bundle, trust, catalog, _ = self.stage()
        operation = {'phase': 'activation_started', 'operation': 'a' * 32}
        order = []
        def preload(*_):
            order.append('preload')
        def start(*args, **kwargs):
            self.assertIsNone(update.active_plugin_directory(self.root))
            order.append('start')
            return True
        def run(args, cwd, **kwargs):
            if args[:3] == ['docker', 'compose', 'up']:
                order.append('compose')
            return b''
        with mock.patch.object(update.Transaction, 'load', return_value=operation), \
                mock.patch.object(update, 'verify_plugin_agent_bundle', return_value=False), \
                mock.patch.object(update, 'optional_plugin_for_activation', return_value=(bundle, trust, catalog, True)), \
                mock.patch.object(update, 'preload_homeassistant_image', side_effect=preload), \
                mock.patch.object(update, 'start_plugin_agent', side_effect=start), \
                mock.patch.object(update, 'run_command', side_effect=run), \
                mock.patch.object(update, 'restart_services_with_replaced_mounts', return_value=[]):
            self.assertTrue(update.activate_native(self.root, str(trust), enable_plugin_bundle=True))
        self.assertEqual(update.active_plugin_directory(self.root), bundle)
        self.assertEqual(order, ['preload', 'start', 'compose'])

    def test_failed_agent_start_does_not_promote_stage(self):
        bundle, trust, catalog, _ = self.stage()
        operation = {'phase': 'activation_started', 'operation': 'a' * 32}
        with mock.patch.object(update.Transaction, 'load', return_value=operation), \
                mock.patch.object(update, 'verify_plugin_agent_bundle', return_value=False), \
                mock.patch.object(update, 'optional_plugin_for_activation', return_value=(bundle, trust, catalog, True)), \
                mock.patch.object(update, 'preload_homeassistant_image'), \
                mock.patch.object(update, 'start_plugin_agent', side_effect=update.UpdateError('agent unavailable')), \
                mock.patch.object(update, 'run_command'), \
                mock.patch.object(update, 'restart_services_with_replaced_mounts', return_value=[]):
            self.assertFalse(update.activate_native(self.root, str(trust), enable_plugin_bundle=True))
        self.assertIsNone(update.active_plugin_directory(self.root))
        self.assertEqual(update.pending_plugin_directory(self.root), bundle)

    def test_pending_bundle_resumes_after_crash_without_downloading(self):
        bundle, trust, catalog, _ = self.stage()
        self.assertIsNone(update.active_plugin_directory(self.root))
        with mock.patch.object(update, 'download', side_effect=AssertionError('must not download')), \
                mock.patch.object(update, 'run_command', return_value=b'linux/amd64'):
            restored = update.prepare_optional_plugin(self.root, str(trust), allow_download=False)
        self.assertEqual(restored[0], bundle)
        self.assertTrue(restored[3])
        self.assertEqual(restored[2], catalog)
        update.record_active_plugin(self.root, restored[0])
        update.clear_pending_plugin(self.root)
        self.assertEqual(update.active_plugin_directory(self.root), bundle)
        self.assertIsNone(update.pending_plugin_directory(self.root))

    def test_expired_pending_bundle_is_not_revived_and_can_be_refetched(self):
        bundle, trust, catalog, _ = self.stage()
        expired = dict(catalog, expires_at=int(time.time()) - 1)
        raw, signature = self.signed(expired)
        (bundle / 'plugins/plugin-catalog.json').write_bytes(raw)
        (bundle / 'plugins/plugin-catalog.json.sig').write_bytes(signature)
        self.assertEqual(update.pending_plugin_directory(self.root), bundle)
        with mock.patch.object(update, 'run_command', return_value=b'linux/amd64'), \
                mock.patch.object(update, 'download', side_effect=update.UpdateError('network unavailable')) as download:
            with self.assertRaisesRegex(update.UpdateError, 'network unavailable'):
                update.prepare_optional_plugin(self.root, str(trust), allow_download=True)
        self.assertEqual(download.call_count, 1)
        self.assertIsNone(update.pending_plugin_directory(self.root))
        self.assertIsNone(update.active_plugin_directory(self.root))
        self.assertTrue(bundle.exists())  # Preserve the expired candidate for diagnosis.

    def test_normal_entrypoint_automatically_reconciles_interrupted_activation(self):
        operation = {'phase': 'activation_started', 'operation': 'a' * 32}
        with mock.patch.object(update, 'Transaction') as transaction_class, \
                mock.patch.object(update.sys, 'argv', ['update.py', '--root', str(self.root)]):
            transaction = transaction_class.return_value
            transaction.load.return_value = operation
            transaction.reconcile_activation.return_value = True
            self.assertEqual(update.main(), 0)
            transaction.reconcile_activation.assert_called_once_with(None, enable_plugin_bundle=True)
            transaction.run.assert_not_called()

    def test_managed_activation_ignores_partial_legacy_bundle_without_blocking_platform(self):
        (self.root / 'bin').mkdir()
        (self.root / 'bin/tp-agent').write_bytes(b'old partial bundle')
        operation = {'phase': 'activation_started', 'operation': 'a' * 32}
        calls = []
        def run(args, cwd, **kwargs):
            calls.append(args)
            return b''
        with mock.patch.object(update.Transaction, 'load', return_value=operation), \
                mock.patch.object(update, 'optional_plugin_for_activation', return_value=None), \
                mock.patch.object(update, 'run_command', side_effect=run), \
                mock.patch.object(update, 'restart_services_with_replaced_mounts', return_value=[]):
            self.assertFalse(update.activate_native(self.root, enable_plugin_bundle=True))
        self.assertTrue(any(args[:3] == ['docker', 'compose', 'up'] for args in calls))


if __name__ == '__main__':
    unittest.main()
