import hashlib
import json
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "config" / "native"))

import release_manifest as rm  # noqa: E402

PUBLISHER = "https://github.com/ThingsPanel/thingspanel-installer/.github/workflows/release.yml"
COMMIT = "a" * 40
NOW = 1_800_000_000


def _openssl(*args, **kwargs):
    return subprocess.run(["openssl", *args], check=True, capture_output=True, **kwargs)


@unittest.skipUnless(shutil.which("openssl"), "openssl is required")
class ReleaseManifestTests(unittest.TestCase):
    """Keys exist only inside each test's temporary directory; no key is a real trust root."""

    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp(prefix="tp-manifest-test-"))
        self.addCleanup(shutil.rmtree, self.tmp, ignore_errors=True)
        self.root, self.priv = self._keypair("root")
        self.payload = self.tmp / "payload"
        (self.payload / "scripts").mkdir(parents=True)
        (self.payload / "install.sh").write_bytes(b"echo install\n")
        (self.payload / "scripts" / "update.py").write_bytes(b"print('update')\n")

    def _keypair(self, name):
        priv = self.tmp / f"{name}.key"
        pub = self.tmp / f"{name}.pub"
        _openssl("ecparam", "-name", "prime256v1", "-genkey", "-noout", "-out", str(priv))
        _openssl("ec", "-in", str(priv), "-pubout", "-out", str(pub))
        return pub, priv

    def _manifest(self, **release_over):
        files = []
        for path in sorted(p for p in self.payload.rglob("*") if p.is_file()):
            data = path.read_bytes()
            files.append(
                {"path": path.relative_to(self.payload).as_posix(), "sha256": hashlib.sha256(data).hexdigest(), "size": len(data)}
            )
        release = {
            "version": "v1.4.0",
            "publisher": PUBLISHER,
            "commit": COMMIT,
            "not_before": NOW - 100,
            "expires_at": NOW + 1000,
        }
        release.update(release_over)
        return json.dumps({"schema": rm.SCHEMA, "release": release, "files": files}, sort_keys=True).encode()

    def _sign(self, manifest, priv=None):
        path = self.tmp / "m.json"
        path.write_bytes(manifest)
        return _openssl("dgst", "-sha256", "-sign", str(priv or self.priv), str(path)).stdout

    def _verify(self, manifest=None, signature=None, **kw):
        manifest = manifest if manifest is not None else self._manifest()
        signature = signature if signature is not None else self._sign(manifest)
        kw.setdefault("expected_publisher", PUBLISHER)
        kw.setdefault("now", NOW)
        return rm.verify_release(manifest, signature, kw.pop("trust_root", self.root), self.payload, **kw)

    def assertRejected(self, reason, **kw):
        with self.assertRaises(rm.ManifestError) as ctx:
            self._verify(**kw)
        self.assertEqual(ctx.exception.reason, reason)

    def test_valid_release_is_accepted(self):
        doc = self._verify(expected_version="v1.4.0")
        self.assertEqual(doc["release"]["commit"], COMMIT)
        self.assertEqual(len(doc["files"]), 2)

    def test_no_trust_root_fails_closed(self):
        self.assertRejected("trust_root_not_configured", trust_root=None)
        self.assertRejected("trust_root_not_configured", trust_root=self.tmp / "absent.pub")

    def test_non_public_key_or_wrong_curve_is_not_a_root(self):
        not_a_key = self.tmp / "junk.pem"
        not_a_key.write_text("hello")
        self.assertRejected("trust_root_not_configured", trust_root=not_a_key)
        rsa = self.tmp / "rsa.pub"
        rsa_key = self.tmp / "rsa.key"
        _openssl("genrsa", "-out", str(rsa_key), "2048")
        _openssl("rsa", "-in", str(rsa_key), "-pubout", "-out", str(rsa))
        self.assertRejected("trust_root_not_configured", trust_root=rsa)

    def test_signature_from_another_key_is_rejected(self):
        _, other_priv = self._keypair("other")
        manifest = self._manifest()
        self.assertRejected("bad_signature", manifest=manifest, signature=self._sign(manifest, other_priv))

    def test_modified_manifest_invalidates_signature(self):
        manifest = self._manifest()
        signature = self._sign(manifest)
        self.assertRejected("bad_signature", manifest=manifest.replace(b"v1.4.0", b"v9.9.9"), signature=signature)

    def test_valid_signature_from_other_publisher_is_rejected(self):
        self.assertRejected("wrong_publisher", manifest=self._manifest(publisher="https://example.com/other.yml"))

    def test_version_mismatch_is_rejected(self):
        self.assertRejected("version_mismatch", expected_version="v1.5.0")

    def test_expired_and_not_yet_valid(self):
        self.assertRejected("expired", now=NOW + 1000)
        self.assertRejected("not_yet_valid", now=NOW - 101)

    def test_revoked_version_is_rejected(self):
        self.assertRejected("revoked", revoked_versions={"v1.4.0"})

    def test_changed_file_is_a_digest_mismatch(self):
        manifest = self._manifest()
        signature = self._sign(manifest)
        (self.payload / "install.sh").write_bytes(b"echo tampered\n")
        self.assertRejected("digest_mismatch", manifest=manifest, signature=signature)

    def test_missing_file_is_rejected(self):
        manifest = self._manifest()
        signature = self._sign(manifest)
        (self.payload / "scripts" / "update.py").unlink()
        self.assertRejected("missing_file", manifest=manifest, signature=signature)

    def test_unlisted_file_is_rejected(self):
        manifest = self._manifest()
        signature = self._sign(manifest)
        (self.payload / "extra.sh").write_bytes(b"echo mixed release\n")
        self.assertRejected("unlisted_file", manifest=manifest, signature=signature)

    def test_symlink_in_payload_is_rejected(self):
        manifest = self._manifest()
        signature = self._sign(manifest)
        (self.payload / "link").symlink_to(self.payload / "install.sh")
        self.assertRejected("unexpected_symlink", manifest=manifest, signature=signature)

    def test_path_traversal_and_absolute_paths_are_rejected(self):
        for bad in ("../escape", "/etc/passwd", "a/../b", "a//b", "./a", "a\\b"):
            manifest = json.dumps(
                {
                    "schema": rm.SCHEMA,
                    "release": {
                        "version": "v1.4.0",
                        "publisher": PUBLISHER,
                        "commit": COMMIT,
                        "not_before": NOW - 100,
                        "expires_at": NOW + 1000,
                    },
                    "files": [{"path": bad, "sha256": "0" * 64, "size": 0}],
                }
            ).encode()
            with self.subTest(path=bad):
                self.assertRejected("bad_path", manifest=manifest)

    def test_duplicate_paths_and_bad_commit_are_rejected(self):
        good = json.loads(self._manifest())
        dup = dict(good, files=good["files"] + [good["files"][0]])
        self.assertRejected("malformed_manifest", manifest=json.dumps(dup).encode())
        bad_commit = json.loads(self._manifest())
        bad_commit["release"]["commit"] = "main"
        self.assertRejected("malformed_manifest", manifest=json.dumps(bad_commit).encode())

    def test_command_line_exit_codes(self):
        import time

        now = int(time.time())
        manifest = self._manifest(not_before=now - 100, expires_at=now + 1000)
        (self.tmp / "release.json").write_bytes(manifest)
        (self.tmp / "release.json.sig").write_bytes(self._sign(manifest))
        base = [
            sys.executable,
            str(Path(rm.__file__)),
            "--manifest",
            str(self.tmp / "release.json"),
            "--signature",
            str(self.tmp / "release.json.sig"),
            "--payload",
            str(self.payload),
            "--publisher",
            PUBLISHER,
        ]
        ok = subprocess.run(base + ["--trust-root", str(self.root)], capture_output=True, text=True)
        self.assertEqual(ok.returncode, 0, ok.stderr)
        self.assertIn("verified (2 files)", ok.stdout)
        no_root = subprocess.run(base, capture_output=True, text=True)
        self.assertEqual(no_root.returncode, 2)
        self.assertIn("trust_root_not_configured", no_root.stderr)
        other_root, _ = self._keypair("other-root")
        wrong_root = subprocess.run(base + ["--trust-root", str(other_root)], capture_output=True, text=True)
        self.assertEqual(wrong_root.returncode, 2)
        self.assertIn("bad_signature", wrong_root.stderr)


if __name__ == "__main__":
    unittest.main()
