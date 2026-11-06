"""reconcile_activation: closes an unresolved activation only after compose up and verification succeed.

The journal and Compose/prepare commands are explicit substitutes; this does not start services.
"""
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

        with mock.patch.object(update, 'run_command', side_effect=fake), \
                mock.patch.object(update, 'restart_services_with_replaced_mounts', return_value=[]):
            tx.reconcile_activation(str(trust_root))
        agent_index = next(i for i, args in enumerate(calls) if 'agent_host.py' in ' '.join(args))
        compose_index = next(i for i, args in enumerate(calls) if args[:3] == ['docker', 'compose', 'up'])
        self.assertLess(agent_index, compose_index)

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
        with mock.patch.object(update, 'run_command') as run:
            self.assertTrue(update.start_plugin_agent(root, str(trust_root)))
        verify_args = run.call_args_list[0].args[0]
        self.assertIn('plugin_catalog.py', ' '.join(verify_args))
        self.assertIn('verify', verify_args)
        self.assertEqual(Path(verify_args[verify_args.index('--trust-root') + 1]), trust_root.resolve())
        args = run.call_args_list[1].args[0]
        self.assertEqual(args[1:3], [str((root / 'tools/agent_host.py').resolve()), 'up'])
        self.assertEqual(Path(args[args.index('--agent-bin') + 1]), (root / 'bin/tp-agent').resolve())
        self.assertEqual(Path(args[args.index('--catalog') + 1]), (root / 'plugins/plugin-catalog.json').resolve())
        self.assertEqual(Path(args[args.index('--trust-root') + 1]), trust_root.resolve())

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
        with mock.patch.object(update, 'run_command', side_effect=update.UpdateError('bad_signature')) as run:
            with self.assertRaisesRegex(update.UpdateError, 'bad_signature'):
                update.start_plugin_agent(root, str(trust_root))
        self.assertEqual(run.call_count, 1)
        self.assertIn('plugin_catalog.py', ' '.join(run.call_args.args[0]))

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
                mock.patch.object(update, 'run_command', side_effect=fake), \
                mock.patch.object(update, 'restart_services_with_replaced_mounts', return_value=[]):
            update.activate_native(root, str(trust_root))
        agent_index = next(i for i, args in enumerate(calls) if 'agent_host.py' in ' '.join(args))
        compose_index = next(i for i, args in enumerate(calls) if args[:3] == ['docker', 'compose', 'up'])
        self.assertLess(agent_index, compose_index)

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
        (stage / 'upgrade.sh').write_text(text)
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
