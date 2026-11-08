"""Signed release verification inside the real update transaction.

The Transaction, verifier and OpenSSL are real. Network download and Compose
rendering are the substitutes (as in test_update_transaction). Keys live only in
each test's temporary directory; none is a trust root.
"""
import copy
import hashlib
import importlib.util
import json
from pathlib import Path
import shutil
import subprocess
import tempfile
import time
import unittest
from unittest import mock

REPO = Path(__file__).resolve().parents[1]
SPEC = importlib.util.spec_from_file_location('tp_update_signed', REPO / 'config/native/update.py')
update = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(update)
BASE = 'https://installer.invalid'
VERSION = 'v-fixture'
PUBLISHER = update.DEFAULT_RELEASE_PUBLISHER
CONFIG = {'services': {'backend': {'image': 'registry/backend:' + VERSION, 'environment': {
    'GOTP_DB_PSQL_HOST': 'postgres', 'GOTP_DB_PSQL_PORT': '5432',
    'GOTP_DB_PSQL_USERNAME': 'postgres', 'GOTP_DB_PSQL_DBNAME': 'ThingsPanel'}},
    'postgres': {}, 'thingsvis-server': {'environment': {
        'DATABASE_URL': 'postgresql://fixture:fixture@postgres/ThingsPanel'}}}}


def openssl(*args):
    return subprocess.run(['openssl', *args], check=True, capture_output=True)


@unittest.skipUnless(shutil.which('openssl'), 'openssl is required')
class SignedUpdateTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='tp-signed-update-')
        self.addCleanup(self.temp.cleanup)
        self.dir = Path(self.temp.name)
        self.root = self.dir / 'install'
        self.root.mkdir()
        patcher = mock.patch.object(update, 'render_with_fallback', return_value=copy.deepcopy(CONFIG))
        patcher.start()
        self.addCleanup(patcher.stop)
        self.pub, self.priv = self.keypair('root')
        self.served = {}  # relative path -> bytes overrides
        self.manifest_overrides = {}
        self.sign_with = self.priv
        self.drop_from_manifest = set()
        self.fetched = []

    def keypair(self, name):
        priv, pub = self.dir / (name + '.key'), self.dir / (name + '.pub')
        openssl('ecparam', '-name', 'prime256v1', '-genkey', '-noout', '-out', str(priv))
        openssl('ec', '-in', str(priv), '-pubout', '-out', str(pub))
        return pub, priv

    def tx(self, policy):
        return update.Transaction(self.root, downloader=self.download, release_policy=policy)

    def policy(self, **kwargs):
        value = {'trust_root': str(self.pub), 'publisher': PUBLISHER, 'revoked': []}
        value.update(kwargs)
        return value

    def release_bytes(self, tx):
        files = []
        for relative, _ in tx.artifacts:
            data = (REPO / relative).read_bytes()
            if relative in self.drop_from_manifest:
                continue
            files.append({'path': relative, 'sha256': hashlib.sha256(data).hexdigest(), 'size': len(data)})
        now = int(time.time())
        release = {'version': VERSION, 'publisher': PUBLISHER, 'commit': 'a' * 40,
                   'not_before': now - 100, 'expires_at': now + 3600}
        release.update(self.manifest_overrides)
        return json.dumps({'schema': 'thingspanel.release-manifest.v1', 'release': release, 'files': files},
                          sort_keys=True).encode()

    def download(self, url, path):
        relative = url[len(BASE) + 1:]
        self.fetched.append(relative)
        path.parent.mkdir(parents=True, exist_ok=True)
        if relative == update.RELEASE_MANIFEST:
            path.write_bytes(self.manifest)
        elif relative == update.RELEASE_MANIFEST + '.sig':
            source = self.dir / 'm.json'
            source.write_bytes(self.manifest)
            path.write_bytes(openssl('dgst', '-sha256', '-sign', str(self.sign_with), str(source)).stdout)
        elif relative in self.served:
            path.write_bytes(self.served[relative])
        else:
            path.write_bytes((REPO / relative).read_bytes())

    def seed(self, tx):
        for relative, _ in tx.artifacts:
            path = self.root / relative
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text('original ' + relative + '\n', encoding='utf-8')
            path.chmod(0o600)
        for relative in update.WATCHED:
            path = self.root / relative
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text('synthetic-local-only\n', encoding='utf-8')
            path.chmod(0o600)
        return {relative: (self.root / relative).read_bytes() for relative, _ in tx.artifacts}

    def run_update(self, policy, **kwargs):
        tx = self.tx(policy)
        before = self.seed(tx)
        self.manifest = self.release_bytes(tx)
        return tx, before

    def assertRejected(self, reason, policy=None, setup=None):
        tx, before = self.run_update(self.policy() if policy is None else policy)
        if setup:
            setup()
        with self.assertRaisesRegex(update.UpdateError, 'Signed release verification failed: ' + reason):
            tx.run(BASE, expected_version=VERSION)
        for relative, content in before.items():
            self.assertEqual((self.root / relative).read_bytes(), content, relative)

    def test_valid_signed_release_is_applied(self):
        tx, before = self.run_update(self.policy())
        tx.run(BASE, expected_version=VERSION)
        for relative, _ in tx.artifacts:
            self.assertEqual((self.root / relative).read_bytes(), (REPO / relative).read_bytes(), relative)
        self.assertIn(update.RELEASE_MANIFEST, self.fetched)

    def test_tampered_served_file_leaves_installation_untouched(self):
        self.served['docker-compose.yml'] = b'services: {}\n'
        self.assertRejected('digest_mismatch')

    def test_manifest_signed_by_another_key_is_rejected(self):
        _, other = self.keypair('other')
        self.sign_with = other
        self.assertRejected('bad_signature')

    def test_staged_file_missing_from_manifest_is_rejected(self):
        self.drop_from_manifest = {'niginx.conf'}
        self.assertRejected('unlisted_file')

    def test_revoked_version_is_rejected(self):
        self.assertRejected('revoked', policy=self.policy(revoked=[VERSION]))

    def test_expired_manifest_is_rejected(self):
        self.manifest_overrides = {'not_before': 1, 'expires_at': 2}
        self.assertRejected('expired')

    def test_other_publisher_is_rejected(self):
        self.manifest_overrides = {'publisher': 'https://example.com/other.yml'}
        self.assertRejected('wrong_publisher')

    def test_requiring_signature_without_trust_root_fails_closed(self):
        self.assertRejected('trust_root_not_configured', policy=self.policy(trust_root=None))

    def test_tampered_verifier_is_never_loaded(self):
        # Hide the genuine sibling by pinning another digest; the download must also match the pin.
        with mock.patch.object(update, 'RELEASE_VERIFIER_SHA256', '0' * 64):
            tx, before = self.run_update(self.policy())
            with self.assertRaisesRegex(update.UpdateError, 'does not match the digest pinned'):
                tx.run(BASE, expected_version=VERSION)
        for relative, content in before.items():
            self.assertEqual((self.root / relative).read_bytes(), content, relative)

    def test_downloaded_verifier_is_used_when_sibling_is_absent(self):
        # The entry points run a lone update.py from a temporary directory.
        lone = self.dir / 'elsewhere' / 'update.py'
        lone.parent.mkdir()
        lone.write_bytes((REPO / 'config/native/update.py').read_bytes())
        spec = importlib.util.spec_from_file_location('tp_update_lone', lone)
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        with mock.patch.object(module, 'render_with_fallback', return_value=copy.deepcopy(CONFIG)):
            tx = module.Transaction(self.root, downloader=self.download, release_policy=self.policy())
            self.seed(tx)
            self.manifest = self.release_bytes(tx)
            self.served['config/native/update.py'] = lone.read_bytes()
            tx.run(BASE, expected_version=VERSION)
        self.assertIn(update.RELEASE_VERIFIER, self.fetched)

    def test_unsigned_mode_is_unchanged(self):
        tx = update.Transaction(self.root, downloader=self.download)
        self.seed(tx)
        tx.run(BASE, expected_version=VERSION)
        self.assertNotIn(update.RELEASE_MANIFEST, self.fetched)


if __name__ == '__main__':
    unittest.main()
