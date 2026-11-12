"""reconcile_activation: closes an unresolved activation only after compose up and verification succeed.

The journal and Compose/prepare commands are explicit substitutes; this does not start services.
"""
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest import mock

REPO = Path(__file__).resolve().parents[1]
SPEC = importlib.util.spec_from_file_location('tp_update_reconcile', REPO / 'config/native/update.py')
update = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(update)


class ReconcileActivation(unittest.TestCase):
    def transaction(self, phase='activation_started'):
        root = Path(tempfile.mkdtemp())
        self.addCleanup(lambda: __import__('shutil').rmtree(root, ignore_errors=True))
        tx = update.Transaction(root)
        data = {'phase': phase, 'operation': 'a' * 32, 'activation_files': {}}
        saved = []
        patches = [mock.patch.object(update.Transaction, 'load', return_value=data),
                   mock.patch.object(update.Transaction, 'save', side_effect=lambda d: saved.append(dict(d)))]
        for p in patches:
            p.start()
            self.addCleanup(p.stop)
        return tx, data, saved

    def test_success_closes_journal_after_up_and_verify(self):
        tx, data, saved = self.transaction()
        with mock.patch.object(update, 'run_command') as run, mock.patch.object(update, 'restart_services_with_replaced_mounts', return_value=[]):
            tx.reconcile_activation()
        commands = [call.args[0] for call in run.call_args_list]
        self.assertIn('--bootstrap', commands[0])
        self.assertEqual(commands[1][:3], ['docker', 'compose', 'up'])
        self.assertIn('--verify', commands[2])
        self.assertEqual(run.call_args_list[2].kwargs['extra_env'], {'TP_INSTALLER_ACTIVE_OPERATION': 'a' * 32})
        self.assertEqual(saved[-1]['phase'], 'complete')

    def test_verification_failure_keeps_journal_unresolved(self):
        tx, data, saved = self.transaction()
        calls = []

        def run(args, root, **kw):
            calls.append(args)
            if '--verify' in args:
                raise update.UpdateError('Installer command failed (exit 1)')
        with mock.patch.object(update, 'run_command', side_effect=run), mock.patch.object(update, 'restart_services_with_replaced_mounts', return_value=[]):
            with self.assertRaises(update.UpdateError):
                tx.reconcile_activation()
        self.assertEqual(saved, [])
        self.assertEqual(data['phase'], 'activation_started')

    def test_reconciliation_starts_agent_before_backend_compose(self):
        tx, _, _ = self.transaction()
        for relative in ('bin/tp-agent', 'plugins/plugin-catalog.json', 'plugins/plugin-catalog.json.sig'):
            path = tx.root / relative
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(b'test fixture')
        trust_root = tx.root.parent / (tx.root.name + '-external-trust.pem')
        trust_root.write_bytes(b'test trust root')
        self.addCleanup(lambda: trust_root.unlink(missing_ok=True))
        calls = []

        def fake(args, cwd, **kw):
            calls.append(args)
            return b''

        with mock.patch.object(update, 'load_verified_plugin_catalog', return_value={'plugins': []}), \
                mock.patch.object(update, 'run_command', side_effect=fake), \
                mock.patch.object(update, 'restart_services_with_replaced_mounts', return_value=[]):
            tx.reconcile_activation(str(trust_root))
        agent_index = next(i for i, args in enumerate(calls) if 'agent_host.py' in ' '.join(args))
        compose_index = next(i for i, args in enumerate(calls) if args[:3] == ['docker', 'compose', 'up'])
        self.assertLess(agent_index, compose_index)
        self.assertIn('--system', calls[agent_index])  # R5a: the Agent is started with the system role

    def test_reconciliation_starts_scope_watcher_after_releasing_update_lock(self):
        tx, _, _ = self.transaction()
        started = []

        def start_watcher(root):
            self.assertEqual(Path(root).resolve(), tx.root.resolve())
            with update.update_lock(tx.state):
                started.append(Path(root).resolve())

        with mock.patch.object(update, 'verify_plugin_agent_bundle', return_value={'plugins': [{'id': 'homeassistant'}]}), \
                mock.patch.object(update, 'preload_homeassistant_image', return_value=True), \
                mock.patch.object(update, 'start_plugin_agent', return_value=True), \
                mock.patch.object(update, 'run_command'), \
                mock.patch.object(update, 'restart_services_with_replaced_mounts', return_value=[]), \
                mock.patch.object(update, 'start_scope_watcher_after_activation', side_effect=start_watcher):
            self.assertTrue(tx.reconcile_activation())
        self.assertEqual(started, [tx.root.resolve()])

    def test_reconciliation_without_agent_does_not_start_scope_watcher(self):
        tx, _, _ = self.transaction()
        with mock.patch.object(update, 'verify_plugin_agent_bundle', return_value=False), \
                mock.patch.object(update, 'run_command'), \
                mock.patch.object(update, 'restart_services_with_replaced_mounts', return_value=[]), \
                mock.patch.object(update, 'start_scope_watcher_after_activation') as watcher:
            self.assertFalse(tx.reconcile_activation())
        watcher.assert_not_called()

    def test_refuses_without_unresolved_activation(self):
        for phase in ('complete', 'recovered', 'config_committed'):
            tx, data, saved = self.transaction(phase)
            with mock.patch.object(update, 'run_command') as run:
                with self.assertRaises(update.UpdateError):
                    tx.reconcile_activation()
            run.assert_not_called()
            self.assertEqual(saved, [])


class PluginAgentActivation(unittest.TestCase):
    def root(self):
        root = Path(tempfile.mkdtemp())
        self.addCleanup(lambda: __import__('shutil').rmtree(root, ignore_errors=True))
        return root

    def add_bundle(self, root):
        for relative in ('bin/tp-agent', 'plugins/plugin-catalog.json', 'plugins/plugin-catalog.json.sig'):
            path = root / relative
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(b'test fixture')

    def external_trust_root(self, root):
        trust_root = root.parent / (root.name + '-external-trust.pem')
        trust_root.write_bytes(b'test trust root')
        self.addCleanup(lambda: trust_root.unlink(missing_ok=True))
        return trust_root

    def test_old_payload_without_bundle_keeps_existing_startup_flow(self):
        root = self.root()
        with mock.patch.object(update, 'run_command') as run:
            self.assertFalse(update.start_plugin_agent(root))
        run.assert_not_called()

    def test_partial_bundle_fails_closed(self):
        root = self.root()
        (root / 'plugins').mkdir()
        (root / 'plugins/plugin-catalog.json').write_bytes(b'catalog')
        with mock.patch.object(update, 'run_command') as run:
            with self.assertRaisesRegex(update.UpdateError, 'bundle is incomplete'):
                update.start_plugin_agent(root)
        run.assert_not_called()

    def test_complete_bundle_starts_agent_with_pinned_files(self):
        root = self.root()
        self.add_bundle(root)
        trust_root = self.external_trust_root(root)
        with mock.patch.object(update, 'load_verified_plugin_catalog', return_value={'plugins': []}) as verify, \
                mock.patch.object(update, 'run_command') as run:
            self.assertTrue(update.start_plugin_agent(root, str(trust_root)))
        verify.assert_called_once_with(root.resolve(), trust_root.resolve())
        args = run.call_args_list[0].args[0]
        self.assertEqual(args[1:3], [str((root / 'tools/agent_host.py').resolve()), 'up'])
        self.assertEqual(Path(args[args.index('--agent-bin') + 1]), (root / 'bin/tp-agent').resolve())
        self.assertEqual(Path(args[args.index('--catalog') + 1]), (root / 'plugins/plugin-catalog.json').resolve())
        self.assertEqual(Path(args[args.index('--trust-root') + 1]), trust_root.resolve())
        self.assertIn('--defer-scope-watcher', args)

    def test_complete_bundle_without_external_trust_root_fails_closed(self):
        root = self.root()
        self.add_bundle(root)
        with mock.patch.object(update, 'run_command') as run:
            with self.assertRaisesRegex(update.UpdateError, 'not supplied out of band'):
                update.start_plugin_agent(root)
        run.assert_not_called()

    def test_bad_catalog_signature_does_not_restart_agent(self):
        root = self.root()
        self.add_bundle(root)
        trust_root = self.external_trust_root(root)
        with mock.patch.object(update, 'load_verified_plugin_catalog', side_effect=update.UpdateError('bad_signature')), \
                mock.patch.object(update, 'run_command') as run:
            with self.assertRaisesRegex(update.UpdateError, 'bad_signature'):
                update.start_plugin_agent(root, str(trust_root))
        run.assert_not_called()

    def test_trust_root_inside_installer_payload_is_rejected(self):
        root = self.root()
        self.add_bundle(root)
        trust_root = root / 'keys/plugin-trust-root.pem'
        trust_root.parent.mkdir(parents=True)
        trust_root.write_bytes(b'test trust root')
        with mock.patch.object(update, 'run_command') as run:
            with self.assertRaisesRegex(update.UpdateError, 'outside the installer payload'):
                update.start_plugin_agent(root, str(trust_root))
        run.assert_not_called()

    def test_activation_starts_agent_before_backend_compose(self):
        root = self.root()
        self.add_bundle(root)
        trust_root = self.external_trust_root(root)
        calls = []

        def fake(args, cwd, **kw):
            calls.append(args)
            return b''

        operation = {'phase': 'activation_started', 'operation': 'b' * 32}
        with mock.patch.object(update.Transaction, 'load', return_value=operation), \
                mock.patch.object(update, 'load_verified_plugin_catalog', return_value={'plugins': []}), \
                mock.patch.object(update, 'run_command', side_effect=fake), \
                mock.patch.object(update, 'restart_services_with_replaced_mounts', return_value=[]):
            update.activate_native(root, str(trust_root))
        agent_index = next(i for i, args in enumerate(calls) if 'agent_host.py' in ' '.join(args))
        compose_index = next(i for i, args in enumerate(calls) if args[:3] == ['docker', 'compose', 'up'])
        self.assertLess(agent_index, compose_index)

    def ha_catalog(self, image_id='sha256:' + 'a' * 64, platform='linux/arm64'):
        return {'plugins': [{'id': 'homeassistant', 'image_id': image_id, 'image_platform': platform,
                            'image_archive_sha256': hashlib.sha256(b'image archive fixture').hexdigest()}]}

    def test_ha_image_archive_is_loaded_then_id_and_platform_are_checked(self):
        root = self.root()
        archive = root / 'plugins/homeassistant-image.tar'
        archive.parent.mkdir(parents=True)
        archive.write_bytes(b'image archive fixture')
        image_id = 'sha256:' + 'a' * 64
        calls = []

        def fake(args, cwd, **kw):
            calls.append(args)
            if args[:3] == ['docker', 'info', '--format']:
                return b'linux/arm64\n'
            if args[:3] == ['docker', 'image', 'inspect']:
                return (image_id + ' linux/arm64\n').encode()
            return b'Loaded image'

        with mock.patch.object(update, 'run_command', side_effect=fake):
            self.assertTrue(update.preload_homeassistant_image(root, self.ha_catalog(image_id)))
        self.assertEqual(calls[0][:3], ['docker', 'info', '--format'])
        self.assertEqual(calls[1][:3], ['docker', 'load', '-i'])
        self.assertEqual(calls[2][:4], ['docker', 'image', 'inspect', '--format'])

    def test_ha_image_archive_is_required_and_missing_fails_closed(self):
        root = self.root()
        with mock.patch.object(update, 'run_command') as run:
            with self.assertRaisesRegex(update.UpdateError, 'archive is missing'):
                update.preload_homeassistant_image(root, self.ha_catalog())
        run.assert_not_called()

    def test_loaded_image_digest_mismatch_fails_closed(self):
        root = self.root()
        archive = root / 'plugins/homeassistant-image.tar'
        archive.parent.mkdir(parents=True)
        archive.write_bytes(b'image archive fixture')
        with mock.patch.object(update, 'run_command', side_effect=[b'linux/arm64', b'Loaded', b'sha256:' + b'b' * 64 + b' linux/arm64']):
            with self.assertRaisesRegex(update.UpdateError, 'digest or platform'):
                update.preload_homeassistant_image(root, self.ha_catalog())

    def test_catalog_architecture_mismatch_with_docker_fails_before_image_load(self):
        root = self.root()
        archive = root / 'plugins/homeassistant-image.tar'
        archive.parent.mkdir(parents=True)
        archive.write_bytes(b'image archive fixture')
        with mock.patch.object(update, 'run_command', return_value=b'linux/amd64') as run:
            with self.assertRaisesRegex(update.UpdateError, 'does not match Docker'):
                update.preload_homeassistant_image(root, self.ha_catalog())
        run.assert_called_once()

    def test_catalog_verification_failure_prevents_native_activation(self):
        root = self.root()
        operation = {'phase': 'activation_started', 'operation': 'b' * 32}
        with mock.patch.object(update.Transaction, 'load', return_value=operation), \
                mock.patch.object(update, 'verify_plugin_agent_bundle', side_effect=update.UpdateError('bad signature')), \
                mock.patch.object(update, 'run_command') as run:
            with self.assertRaisesRegex(update.UpdateError, 'bad signature'):
                update.activate_native(root, '/outside/trust-root.pem')
        run.assert_not_called()

    def test_image_preload_failure_prevents_agent_and_backend_start(self):
        root = self.root()
        operation = {'phase': 'activation_started', 'operation': 'b' * 32}
        with mock.patch.object(update.Transaction, 'load', return_value=operation), \
                mock.patch.object(update, 'verify_plugin_agent_bundle', return_value=self.ha_catalog()), \
                mock.patch.object(update, 'preload_homeassistant_image', side_effect=update.UpdateError('digest mismatch')), \
                mock.patch.object(update, 'run_command') as run:
            with self.assertRaisesRegex(update.UpdateError, 'digest mismatch'):
                update.activate_native(root, '/outside/trust-root.pem')
        run.assert_not_called()

    def test_tampered_archive_rejected_before_docker_load(self):
        root = self.root()
        archive = root / 'plugins/homeassistant-image.tar'
        archive.parent.mkdir(parents=True)
        archive.write_bytes(b'tampered archive')
        # Even if the catalog's image is already present, replacing the signed archive
        # must fail before Docker is queried or asked to load it.
        with mock.patch.object(update, 'run_command', return_value=b'sha256:' + b'a' * 64 + b' linux/arm64') as run:
            with self.assertRaisesRegex(update.UpdateError, 'archive digest does not match'):
                update.preload_homeassistant_image(root, self.ha_catalog())
        run.assert_not_called()

    def test_invalid_archive_digest_format_fails_before_any_docker_command(self):
        root = self.root()
        archive = root / 'plugins/homeassistant-image.tar'
        archive.parent.mkdir(parents=True)
        archive.write_bytes(b'image archive fixture')
        catalog = self.ha_catalog()
        catalog['plugins'][0]['image_archive_sha256'] = 'A' * 64
        with mock.patch.object(update, 'run_command') as run:
            with self.assertRaisesRegex(update.UpdateError, 'archive digest is invalid'):
                update.preload_homeassistant_image(root, catalog)
        run.assert_not_called()

    def test_activation_preloads_image_before_prepare_agent_and_compose(self):
        root = self.root()
        archive = root / 'plugins/homeassistant-image.tar'
        archive.parent.mkdir(parents=True)
        archive.write_bytes(b'image archive fixture')
        operation = {'phase': 'activation_started', 'operation': 'b' * 32}
        image_id = 'sha256:' + 'a' * 64
        calls = []

        def fake(args, cwd, **kw):
            calls.append(args)
            if args[:3] == ['docker', 'info', '--format']:
                return b'linux/arm64'
            if args[:3] == ['docker', 'image', 'inspect']:
                return (image_id + ' linux/arm64').encode()
            return b''

        with mock.patch.object(update.Transaction, 'load', return_value=operation), \
                mock.patch.object(update, 'verify_plugin_agent_bundle', return_value=self.ha_catalog(image_id)), \
                mock.patch.object(update, 'run_command', side_effect=fake), \
                mock.patch.object(update, 'restart_services_with_replaced_mounts', return_value=[]):
            update.activate_native(root, '/outside/trust-root.pem')
        image_load = next(i for i, args in enumerate(calls) if args[:3] == ['docker', 'load', '-i'])
        image_inspect = next(i for i, args in enumerate(calls) if args[:3] == ['docker', 'image', 'inspect'])
        prepare = next(i for i, args in enumerate(calls) if 'prepare.py' in ' '.join(args))
        agent = next(i for i, args in enumerate(calls) if 'agent_host.py' in ' '.join(args))
        compose = next(i for i, args in enumerate(calls) if args[:3] == ['docker', 'compose', 'up'])
        self.assertLess(image_load, image_inspect)
        self.assertLess(image_inspect, prepare)
        self.assertLess(prepare, agent)
        self.assertLess(agent, compose)

class ReplacedMounts(unittest.TestCase):
    def run_case(self, file_offset):
        root = Path(tempfile.mkdtemp())
        self.addCleanup(lambda: __import__('shutil').rmtree(root, ignore_errors=True))
        mounted = root / 'runtime.json'
        mounted.write_text('{}')
        started = __import__('time').time() - 1000
        os.utime(mounted, (started + file_offset, started + file_offset))
        stamp = __import__('datetime').datetime.fromtimestamp(started, __import__('datetime').timezone.utc).strftime('%Y-%m-%dT%H:%M:%S.123456789Z')
        config = {'services': {'notification-core': {'volumes': [{'type': 'bind', 'source': str(mounted), 'target': '/run/config/runtime.json'}]},
                               'redis': {'volumes': [{'type': 'volume', 'source': 'data', 'target': '/data'}]}}}
        calls = []

        def fake(args, cwd, **kw):
            calls.append(args)
            if args[:3] == ['docker', 'compose', 'config']:
                return json.dumps(config).encode()
            if args[:3] == ['docker', 'compose', 'ps']:
                return b'cid1\n'
            if args[:2] == ['docker', 'inspect']:
                return ('true ' + stamp).encode()
            return b''
        with mock.patch.object(update, 'run_command', side_effect=fake):
            stale = update.restart_services_with_replaced_mounts(root)
        return stale, calls

    def test_file_replaced_after_start_restarts_only_that_service(self):
        stale, calls = self.run_case(100)
        self.assertEqual(stale, ['notification-core'])
        self.assertIn(['docker', 'compose', 'restart', 'notification-core'], calls)

    def test_file_older_than_container_start_restarts_nothing(self):
        stale, calls = self.run_case(-100)
        self.assertEqual(stale, [])
        self.assertFalse(any(c[:3] == ['docker', 'compose', 'restart'] for c in calls))


class BackendImageVersion(unittest.TestCase):
    def check(self, image, version='v1.3.0'):
        root = Path(tempfile.mkdtemp())
        self.addCleanup(lambda: __import__('shutil').rmtree(root, ignore_errors=True))
        tx = update.Transaction(root)
        stage = root / 'stage'
        stage.mkdir()
        config = {'services': {'backend': {'image': image}}}
        text = 'UPDATE_ENGINE_SHA256="' + __import__('hashlib').sha256(Path(update.__file__).read_bytes()).hexdigest() + '"\n'
        entrypoint = 'upgrade.ps1' if os.name == 'nt' else 'upgrade.sh'
        (stage / entrypoint).write_text(text)
        tx.artifacts = []
        with mock.patch.object(update, 'render_with_fallback', return_value=config):
            tx.validate(stage, version)

    def test_uninterpolated_default_image_matches_requested_version(self):
        self.check('${TP_BACKEND_IMAGE:-registry.example/thingspanel-go:v1.3.0}')

    def test_uninterpolated_default_image_with_other_version_is_rejected(self):
        with self.assertRaises(update.UpdateError):
            self.check('${TP_BACKEND_IMAGE:-registry.example/thingspanel-go:v1.2.0}')


if __name__ == '__main__':
    unittest.main()
