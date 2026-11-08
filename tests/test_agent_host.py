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


if __name__ == '__main__':
    unittest.main()
