#!/usr/bin/env python3
"""Drives update.py / release_manifest.py for the Go interoperability tests (agent/config_interop_test.go).

    py_txn.py run     ROOT PAYLOAD.json MODE EVENT   MODE: kill | recover | run | hold
    py_txn.py trylock STATE                          prints OK or BUSY
    py_txn.py lock    STATE                          holds the update lock, prints READY, exits on stdin EOF
    py_txn.py verify  TRUST MANIFEST SIG PUBLISHER VERSION REVOKED NOW FILES.json
"""
import importlib.util
import json
import os
from pathlib import Path
import signal
import sys
from unittest import mock

NATIVE = Path(os.environ['TP_NATIVE_DIR'])
BASE = 'https://updates.example.invalid/release'


def load(name):
    spec = importlib.util.spec_from_file_location(name, NATIVE / (name + '.py'))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def main(argv):
    command = argv[0]
    if command == 'verify':
        trust, manifest, sig, publisher, version, revoked, now, files = argv[1:]
        verifier = load('release_manifest')
        try:
            verifier.verify_files(Path(manifest).read_bytes(), Path(sig).read_bytes(), trust or None,
                                  json.loads(Path(files).read_text()), expected_publisher=publisher,
                                  expected_version=version or None,
                                  revoked_versions=frozenset(x for x in revoked.split(',') if x), now=int(now))
        except verifier.ManifestError as error:
            print(error.reason)
            return 0
        print('OK')
        return 0
    update = load('update')
    if command in ('lock', 'trylock'):
        state = Path(argv[1])
        try:
            with update.update_lock(state):
                if command == 'trylock':
                    print('OK')
                else:
                    print('READY', flush=True)
                    sys.stdin.read()
        except update.UpdateError:
            print('BUSY')
        return 0
    root, payload_file, mode, event = argv[1:]
    payload = json.loads(Path(payload_file).read_text())

    def downloader(url, path):
        update.make_directories(path.parent)
        path.write_bytes(payload[url[len(BASE) + 1:]].encode())

    def checkpoint(name):
        if name != event:
            return
        if mode == 'kill':
            os.kill(os.getpid(), signal.SIGKILL)
        elif mode == 'hold':
            (Path(root) / '.worker-ready').write_text('ready')
            sys.stdin.readline()

    with mock.patch.object(update, 'render_with_fallback', return_value={}):
        transaction = update.Transaction(Path(root), downloader=downloader, checkpoint=checkpoint,
                                         validator=lambda stage, version: None)
        transaction.run(BASE, recovery_only=(mode == 'recover'))
    return 0


if __name__ == '__main__':
    try:
        sys.exit(main(sys.argv[1:]))
    except Exception as error:  # noqa: BLE001 - test driver
        print('PYERROR %s: %s' % (type(error).__name__, error))
        sys.exit(3)
