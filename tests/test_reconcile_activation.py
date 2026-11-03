"""reconcile_activation: closes an unresolved activation only after compose up and verification succeed.

The journal and Compose/prepare commands are explicit substitutes; this does not start services.
"""
import importlib.util
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
        with mock.patch.object(update, 'run_command') as run:
            tx.reconcile_activation()
        commands = [call.args[0] for call in run.call_args_list]
        self.assertEqual(commands[0][:3], ['docker', 'compose', 'up'])
        self.assertIn('--verify', commands[1])
        self.assertEqual(run.call_args_list[1].kwargs['extra_env'], {'TP_INSTALLER_ACTIVE_OPERATION': 'a' * 32})
        self.assertEqual(saved[-1]['phase'], 'complete')

    def test_verification_failure_keeps_journal_unresolved(self):
        tx, data, saved = self.transaction()
        calls = []

        def run(args, root, **kw):
            calls.append(args)
            if '--verify' in args:
                raise update.UpdateError('Installer command failed (exit 1)')
        with mock.patch.object(update, 'run_command', side_effect=run):
            with self.assertRaises(update.UpdateError):
                tx.reconcile_activation()
        self.assertEqual(saved, [])
        self.assertEqual(data['phase'], 'activation_started')

    def test_refuses_without_unresolved_activation(self):
        for phase in ('complete', 'recovered', 'config_committed'):
            tx, data, saved = self.transaction(phase)
            with mock.patch.object(update, 'run_command') as run:
                with self.assertRaises(update.UpdateError):
                    tx.reconcile_activation()
            run.assert_not_called()
            self.assertEqual(saved, [])


if __name__ == '__main__':
    unittest.main()
