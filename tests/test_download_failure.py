"""Synthetic entry-point checks. No database, daemon, network or real installation.

Point TP_STAGE_INSTALLER_SOURCE at a reviewed candidate checkout. Fake curl and
Docker are confined to a private TemporaryDirectory. This test documents only
pre-replacement download failures; crash recovery requires the extension cases
listed in README.md and must use the candidate's actual transaction hook.
"""
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

SOURCE = Path(os.environ.get('TP_STAGE_INSTALLER_SOURCE', Path(__file__).resolve().parents[1]))
FILES = ('docker-compose.yml', 'niginx.conf', 'config/native/prepare.py',
         'config/native/runtime.encore.json', 'config/native/runtime.notification.json',
         'config/native/tick.sh', 'config/native/README.md')
OLD = {name: ('old fixture: ' + name + '\n').encode() for name in FILES}
OLD['.env'] = b'TP_DATABASE_PASSWORD=fixture-only\n'
OLD['config/native/plugin-token'] = b'fixture-token\n'
CONFIG = {'services': {'backend': {'environment': {
    'GOTP_DB_PSQL_HOST': 'postgres', 'GOTP_DB_PSQL_PORT': '5432',
    'GOTP_DB_PSQL_USERNAME': 'postgres', 'GOTP_DB_PSQL_DBNAME': 'ThingsPanel'}},
    'postgres': {}, 'thingsvis-server': {'environment': {
        'DATABASE_URL': 'postgresql://fixture:fixture@postgres/ThingsPanel'}}}}

DOCKER = r'''#!/usr/bin/env python3
import os, sys
from pathlib import Path
args = sys.argv[1:]
with open(os.environ['TP_FAKE_DOCKER_LOG'], 'a') as f: f.write(' '.join(args) + '\n')
if args and args[0] == 'version': print('25.0.0'); sys.exit(0)
if len(args) >= 2 and args[:2] == ['compose','version']: print('2.20.0'); sys.exit(0)
if args and args[0] == 'compose' and 'config' in args:
    print(Path(os.environ['TP_FAKE_CONFIG']).read_text()); sys.exit(0)
sys.exit(93)
'''
CURL = r'''#!/usr/bin/env python3
import json, os, pathlib, sys
from urllib.parse import urlsplit
root = pathlib.Path(os.environ['TP_FIXTURE_ROOT']).resolve()
count_file = root / 'curl-count'
count = int(count_file.read_text()) + 1 if count_file.exists() else 1
count_file.write_text(str(count))
args = sys.argv[1:]
urls = [arg for arg in args if arg.startswith('https://')]
if len(urls) != 1: sys.exit(96)
url = urlsplit(urls[0])
if url.scheme != 'https' or url.netloc != 'install.thingspanel.io': sys.exit(97)
with (root / 'curl-events.jsonl').open('a') as stream:
    stream.write(json.dumps({'call': count, 'path': url.path.lstrip('/')}) + '\n')
try: dest = pathlib.Path(args[args.index('-o') + 1]).resolve()
except (ValueError, IndexError): sys.exit(94)
try: dest.relative_to(root)
except ValueError: sys.exit(95)
if count == int(os.environ['TP_FAIL_DOWNLOAD']):
    dest.write_bytes(b'partial download')
    sys.exit(42)
name = dest.name
content = b'fixture\n'
if name.endswith('.json'): content = b'{}\n'
if name == 'prepare.py': content = b'raise SystemExit("fixture prepare must not run")\n'
dest.write_bytes(content)
'''

@unittest.skipIf(os.name == 'nt', 'Bash entrypoint tests run on Linux and macOS')
class DownloadFailurePreservation(unittest.TestCase):
    def case(self, entry, failed_call, bootstrap=None):
        with tempfile.TemporaryDirectory(prefix='tp-stage-synthetic-') as name:
            root = Path(name)
            target = root / 'install'
            bindir = root / 'bin'
            target.mkdir(); bindir.mkdir()
            for filename, content in OLD.items():
                path = target / filename
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_bytes(content); path.chmod(0o640)
            script = target / entry
            shutil.copyfile(SOURCE / entry, script)
            # The reviewed, digest-pinned engine is present in this fixture.
            # Bootstrap download/digest rejection has its own test. These
            # fourteen cases retain the original payload-download boundaries.
            shutil.copyfile(SOURCE / 'config/native/update.py', target / 'config/native/update.py')
            if bootstrap is not None:
                (target / 'config/native/update.py').unlink()
            for program, source in [('docker', DOCKER), ('curl', CURL), ('wget', '#!/bin/sh\nexit 92\n')]:
                path = bindir / program; path.write_text(source); path.chmod(0o755)
            config = root / 'config.json'; config.write_text(json.dumps(CONFIG))
            env = os.environ.copy()
            env.update(PATH=str(bindir) + os.pathsep + env['PATH'], INSTALL_DIR=str(target),
                       TMPDIR=str(root), TP_FIXTURE_ROOT=str(root), TP_FAIL_DOWNLOAD=str(999 if bootstrap == 'digest' else failed_call),
                       TP_FAKE_CONFIG=str(config), TP_FAKE_DOCKER_LOG=str(root / 'docker.log'))
            argv = ['bash', str(script)] + (['v-fixture'] if entry == 'upgrade.sh' else [])
            result = subprocess.run(argv, cwd=target, env=env, capture_output=True, text=True, timeout=20)
            self.assertNotEqual(result.returncode, 0, 'injected download failure unexpectedly succeeded')
            self.assertTrue((root / 'curl-count').exists(), 'entry did not reach the injected download boundary')
            self.assertEqual(int((root / 'curl-count').read_text()), failed_call, 'wrong boundary reached')
            events = [json.loads(line) for line in (root / 'curl-events.jsonl').read_text().splitlines()]
            expected = ['config/native/update.py'] if bootstrap is not None else list(FILES[:failed_call])
            self.assertEqual(events, [{'call': index + 1, 'path': path} for index, path in enumerate(expected)],
                             'failure did not reach the intended payload URL sequence')
            for filename, original in OLD.items():
                path = target / filename
                self.assertTrue(path.is_file(), f'{filename} disappeared')
                self.assertEqual(path.read_bytes(), original, f'{entry}: {filename} changed on download failure {failed_call}')
                self.assertEqual(path.stat().st_mode & 0o777, 0o640, f'{filename} mode changed')
            log = (root / 'docker.log').read_text() if (root / 'docker.log').exists() else ''
            self.assertNotIn('compose up', log)

    def test_upgrade_each_download_failure_preserves_old_files(self):
        for nth in range(1, 8):
            with self.subTest(download=nth): self.case('upgrade.sh', nth)

    def test_install_each_download_failure_preserves_old_files(self):
        for nth in range(1, 8):
            with self.subTest(download=nth): self.case('install.bash', nth)

    def test_bootstrap_download_failure_and_digest_mismatch_stop_before_payload(self):
        for entry in ('install.bash', 'upgrade.sh'):
            for boundary in ('download', 'digest'):
                with self.subTest(entry=entry, boundary=boundary):
                    self.case(entry, 1, bootstrap=boundary)

if __name__ == '__main__': unittest.main()
