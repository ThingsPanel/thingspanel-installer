"""Child process helper for isolated transaction crash/lock tests only."""
import os
from pathlib import Path
import signal
import sys
from unittest import mock
from test_update_transaction import BASE, CONFIG, payload_download, update

root, mode, target_event = sys.argv[1:]


def checkpoint(event):
    if event != target_event:
        return
    if mode in ('kill', 'kill-recover', 'kill-env', 'kill-broker'):
        os.kill(os.getpid(), signal.SIGKILL)
    elif mode == 'terminate':
        os.kill(os.getpid(), signal.SIGTERM)
    elif mode == 'hold':
        (Path(root) / '.worker-ready').write_text('ready')
        sys.stdin.readline()


with mock.patch.object(update, 'render_with_fallback', return_value=CONFIG):
    transaction = update.Transaction(Path(root), downloader=payload_download, checkpoint=checkpoint)
    activation = (lambda _: transaction.write_activation_environment(b'TP_AUTH=synthetic-new-identity\n')) if mode == 'kill-env' else (lambda _: None)
    if mode == 'kill-broker':
        activation = lambda _: transaction.write_activation_file('config/native/broker/plugin-token', b'synthetic-broker-identity-not-a-real-secret', 0o440)
    transaction.run(BASE, activate=activation,
                    recovery_only=mode in ('recover', 'kill-recover'))
