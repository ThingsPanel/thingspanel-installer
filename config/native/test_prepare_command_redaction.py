import subprocess
import unittest
from unittest import mock

from config.native import prepare


class CommandRedactionTest(unittest.TestCase):
    def test_error_does_not_expose_subprocess_arguments_or_stderr(self):
        secret = 'fixture-password-do-not-use'
        result = subprocess.CompletedProcess([], 17, stdout=b'', stderr=f'FATAL: database URL postgres://user:{secret}@host/db'.encode())
        with mock.patch.object(prepare.subprocess, 'run', return_value=result):
            with self.assertRaises(RuntimeError) as caught:
                prepare.command('docker', 'compose', secret, 'config')
        self.assertNotIn(secret, str(caught.exception))
        self.assertNotIn('postgres://', str(caught.exception))
        self.assertIn('17', str(caught.exception))

    def test_success_returns_command_output(self):
        result = subprocess.CompletedProcess([], 0, stdout=b'output\n', stderr=b'')
        with mock.patch.object(prepare.subprocess, 'run', return_value=result):
            self.assertEqual(prepare.command('fixture'), 'output')


if __name__ == '__main__':
    unittest.main()
