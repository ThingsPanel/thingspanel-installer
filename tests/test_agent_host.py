import importlib.util
from pathlib import Path
import sys
import unittest
from unittest import mock

REPO = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(REPO / 'tools'))
import agent_host  # noqa: E402


class AgentHost(unittest.TestCase):
    def test_installer_defers_scope_watcher_without_waiting_or_lock_attempts(self):
        with mock.patch.object(agent_host.scope_sync, 'ensure') as ensure, \
                mock.patch.object(agent_host.time, 'sleep', side_effect=AssertionError('must not sleep')):
            status = agent_host.ensure_scope_watcher('/stack', '/agent', deferred=True)
        self.assertEqual(status, 'deferred')
        ensure.assert_not_called()


class SystemRole(unittest.TestCase):
    def args(self, root, **extra):
        values = dict(stack_dir=str(root / 'stack'), agent_dir=str(root / 'agent'), agent_bin=str(root / 'tp-agent'),
                      catalog=str(root / 'plugin-catalog.json'), trust_root=str(root / 'root.pem'), agent_image='img:1',
                      network=None, system=True, defer_scope_watcher=True,
                      release_source=None, release_trust_root=None, release_ca=None)
        values.update(extra)
        return mock.Mock(**values)

    def run_up(self, system):
        import tempfile
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / 'stack').mkdir()
            (root / 'stack/.env').write_text('X=1\n')
            for name in ('tp-agent', 'plugin-catalog.json', 'plugin-catalog.json.sig', 'root.pem'):
                (root / name).write_text('x')
            calls = []

            def fake_sh(args, check=True):
                calls.append(args)
                (root / 'agent/run').mkdir(parents=True, exist_ok=True)
                (root / 'agent/run/agent.sock').write_text('')
                return mock.Mock(returncode=0, stdout='', stderr='')
            with mock.patch.object(agent_host, 'sh', fake_sh), mock.patch.object(agent_host.scope_sync, 'stop') as stop, \
                    mock.patch.object(agent_host.scope_sync, 'ensure') as ensure:
                agent_host.up(self.args(root, system=system))
            return calls, stop, ensure, root

    def test_system_role_is_wired_with_its_own_socket_token_and_never_in_run_dir(self):
        calls, stop, ensure, root = self.run_up(True)
        run = next(c for c in calls if c[:3] == ['docker', 'run', '-d'])
        joined = ' '.join(run)
        self.assertIn('-system-socket /run/agent/system/agent-system.sock', joined)
        self.assertIn('-system-token-file /run/agent/system/token', joined)
        self.assertIn('-system-config /run/agent/system/system.json', joined)
        # The platform backend mounts only agent/run; the system socket must live in a different directory.
        self.assertIn('%s:/run/agent/system' % (root / 'agent/system').resolve(), joined)
        self.assertEqual(run[run.index('-socket') + 1], '/run/agent-ctl/agent.sock')   # plugin socket stays in run/
        self.assertNotIn('agent-system.sock', str((root / 'agent/run').resolve()))
        self.assertIn('--network', run)
        provision = next(c for c in calls if '-provision-system' in c)
        self.assertEqual(provision[provision.index('-provision-system') + 1], '/run/agent/system')
        stop.assert_called_once()          # the Python watcher is stopped, not started beside the Agent
        ensure.assert_not_called()

    def test_release_source_enables_upgrade_only_with_a_trust_root(self):
        import tempfile
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / 'stack').mkdir()
            with mock.patch.object(agent_host, 'sh', return_value=mock.Mock()):
                with self.assertRaisesRegex(RuntimeError, 'unsigned upgrades are not allowed'):
                    agent_host.provision_system(self.args(root, release_source='https://updates.example/release'), root / 'agent', 'p')
                calls = []
                with mock.patch.object(agent_host, 'sh', lambda a, check=True: calls.append(a)):
                    mounts, _ = agent_host.provision_system(self.args(root, release_source='https://updates.example/release',
                                                                      release_trust_root=str(root / 'root.pem')), root / 'agent', 'p')
                provision = calls[0]
                self.assertEqual(provision[provision.index('-release-source') + 1], 'https://updates.example/release')
                self.assertIn('-host-root', provision)
                self.assertTrue(any(m.endswith(':/run/agent/release-root.pem:ro') for m in mounts))

    def test_without_system_flag_nothing_changes(self):
        calls, stop, ensure, root = self.run_up(False)
        run = next(c for c in calls if c[:3] == ['docker', 'run', '-d'])
        self.assertNotIn('-system-socket', run)
        self.assertFalse(any('-provision-system' in c for c in calls))
        stop.assert_not_called()

    def test_scope_sync_defers_to_the_agent_when_system_role_is_provisioned(self):
        import tempfile
        with tempfile.TemporaryDirectory() as tmp:
            state = Path(tmp)
            (state / 'system').mkdir()
            (state / 'system/system.json').write_text('{}')
            with mock.patch.object(agent_host.scope_sync, 'stop') as stop:
                spawn = mock.Mock(side_effect=AssertionError('must not spawn a second watcher'))
                self.assertEqual(agent_host.scope_sync.ensure('/stack', state, spawn=spawn), 'running')
            stop.assert_called_once_with(state)


if __name__ == '__main__':
    unittest.main()
