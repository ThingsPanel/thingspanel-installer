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
    if mode in ('kill', 'kill-recover'):
        os.kill(os.getpid(), signal.SIGKILL)
    elif mode == 'hold':
        (Path(root) / '.worker-ready').write_text('ready')
        sys.stdin.readline()


with mock.patch.object(update, 'render_with_fallback', return_value=CONFIG):
    transaction = update.Transaction(Path(root), downloader=payload_download, checkpoint=checkpoint)
    transaction.run(BASE, activate=lambda _: None,
                    recovery_only=mode in ('recover', 'kill-recover'))
