#!/usr/bin/env python3
"""Verify a signed ThingsPanel release manifest before any payload is used.

The manifest is signed once the payload is frozen; the payload never embeds the
manifest's own digest, so installer, updater and management scripts do not
depend on each other circularly.

Trust model
-----------
* The only trust anchor is an ECDSA P-256 public key supplied by the caller
  (a pinned PEM file). Nothing inside the manifest or payload can introduce or
  replace it.
* No root key ships with this repository. Without a configured root every
  verification fails closed with ``TrustRootNotConfigured``.
* A valid signature is necessary but not sufficient: the publisher, version,
  validity window, revocation list, and the exact set of files (path, size,
  SHA-256) must all match.

Signature format: DER ECDSA over SHA-256 of the exact manifest bytes, as produced by
``openssl dgst -sha256 -sign key.pem manifest.json > manifest.json.sig``.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import time
from pathlib import Path, PurePosixPath

SCHEMA = "thingspanel.release-manifest.v1"
_HEX64 = re.compile(r"^[0-9a-f]{64}$")
_COMMIT = re.compile(r"^[0-9a-f]{40}$")
_CHUNK = 1024 * 1024


class ManifestError(Exception):
    """The manifest, its signature, or the payload cannot be trusted."""

    def __init__(self, reason: str, detail: str = ""):
        super().__init__(f"{reason}: {detail}" if detail else reason)
        self.reason = reason


class TrustRootNotConfigured(ManifestError):
    def __init__(self, detail: str = ""):
        super().__init__("trust_root_not_configured", detail)


def _openssl() -> str:
    path = shutil.which("openssl")
    if not path:
        raise ManifestError("openssl_unavailable", "openssl is required to verify release signatures")
    return path


def load_trust_root(path: os.PathLike | str | None) -> Path:
    """Return the pinned root key path, or fail closed when none is configured."""
    if not path:
        raise TrustRootNotConfigured("no release trust root was provided")
    root = Path(path)
    if not root.is_file():
        raise TrustRootNotConfigured(f"trust root file not found: {root}")
    text = root.read_text(encoding="utf-8", errors="replace")
    if "BEGIN PUBLIC KEY" not in text:
        raise TrustRootNotConfigured("trust root is not a PEM public key")
    proc = subprocess.run(
        [_openssl(), "pkey", "-pubin", "-in", str(root), "-text", "-noout"],
        capture_output=True,
        text=True,
        check=False,
    )
    if proc.returncode != 0:
        raise TrustRootNotConfigured("trust root is not a readable public key")
    if "prime256v1" not in proc.stdout and "P-256" not in proc.stdout:
        raise TrustRootNotConfigured("trust root must be an ECDSA P-256 public key")
    return root


def verify_signature(manifest: bytes, signature: bytes, root: Path) -> None:
    with tempfile.TemporaryDirectory(prefix="tp-manifest-") as tmp:
        manifest_path = Path(tmp) / "manifest"
        signature_path = Path(tmp) / "signature"
        manifest_path.write_bytes(manifest)
        signature_path.write_bytes(signature)
        proc = subprocess.run(
            [_openssl(), "dgst", "-sha256", "-verify", str(root), "-signature", str(signature_path), str(manifest_path)],
            capture_output=True,
            text=True,
            check=False,
        )
    if proc.returncode != 0 or "Verified OK" not in proc.stdout:
        raise ManifestError("bad_signature", "manifest signature does not verify against the pinned trust root")


def _parse(manifest: bytes) -> dict:
    try:
        doc = json.loads(manifest.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise ManifestError("malformed_manifest", str(exc)) from exc
    if not isinstance(doc, dict) or doc.get("schema") != SCHEMA:
        raise ManifestError("malformed_manifest", "unsupported schema")
    release = doc.get("release")
    files = doc.get("files")
    if not isinstance(release, dict) or not isinstance(files, list) or not files:
        raise ManifestError("malformed_manifest", "release and a non-empty files list are required")
    for key in ("version", "publisher"):
        if not isinstance(release.get(key), str) or not release[key]:
            raise ManifestError("malformed_manifest", f"release.{key} is required")
    if not isinstance(release.get("commit"), str) or not _COMMIT.match(release["commit"]):
        raise ManifestError("malformed_manifest", "release.commit must be a 40-hex commit id")
    for key in ("not_before", "expires_at"):
        if not isinstance(release.get(key), int) or isinstance(release.get(key), bool):
            raise ManifestError("malformed_manifest", f"release.{key} must be an integer epoch")
    return doc


def _clean_path(raw: object) -> str:
    if not isinstance(raw, str) or not raw or "\\" in raw or "\x00" in raw:
        raise ManifestError("bad_path", f"invalid payload path: {raw!r}")
    pure = PurePosixPath(raw)
    if pure.is_absolute() or any(part in ("", ".", "..") for part in pure.parts) or str(pure) != raw:
        raise ManifestError("bad_path", f"payload path must be a clean relative path: {raw!r}")
    return raw


def _sha256(path: Path) -> tuple[str, int]:
    digest = hashlib.sha256()
    size = 0
    with path.open("rb") as handle:
        while True:
            chunk = handle.read(_CHUNK)
            if not chunk:
                break
            digest.update(chunk)
            size += len(chunk)
    return digest.hexdigest(), size


def _walk(payload: Path) -> set[str]:
    found: set[str] = set()
    for current, dirs, names in os.walk(payload, followlinks=False):
        for name in list(dirs) + names:
            if (Path(current) / name).is_symlink():
                raise ManifestError("unexpected_symlink", f"symlink in payload: {Path(current) / name}")
        for name in names:
            found.add(Path(current, name).relative_to(payload).as_posix())
    return found


def _verify_header(manifest, signature, trust_root, *, expected_publisher, expected_version, revoked_versions, now):
    """Signature, identity, validity window and parsed file table (path -> (sha256, size))."""
    root = load_trust_root(trust_root)
    verify_signature(manifest, signature, root)
    doc = _parse(manifest)
    release = doc["release"]

    if release["publisher"] != expected_publisher:
        raise ManifestError("wrong_publisher", f"manifest publisher {release['publisher']!r} is not the expected one")
    if expected_version is not None and release["version"] != expected_version:
        raise ManifestError("version_mismatch", f"manifest version {release['version']!r} != {expected_version!r}")
    if release["version"] in revoked_versions or release["version"] in set(doc.get("revoked_versions") or []):
        raise ManifestError("revoked", f"release {release['version']!r} has been withdrawn")
    current = int(time.time()) if now is None else now
    if current < release["not_before"]:
        raise ManifestError("not_yet_valid", "release is not valid yet")
    if current >= release["expires_at"]:
        raise ManifestError("expired", "release manifest has expired")

    expected: dict[str, tuple[str, int]] = {}
    for entry in doc["files"]:
        if not isinstance(entry, dict):
            raise ManifestError("malformed_manifest", "file entries must be objects")
        rel = _clean_path(entry.get("path"))
        digest, size = entry.get("sha256"), entry.get("size")
        if not isinstance(digest, str) or not _HEX64.match(digest) or not isinstance(size, int) or isinstance(size, bool) or size < 0:
            raise ManifestError("malformed_manifest", f"bad digest or size for {rel}")
        if rel in expected:
            raise ManifestError("malformed_manifest", f"duplicate path {rel}")
        expected[rel] = (digest, size)
    return doc, expected


def verify_release(
    manifest: bytes,
    signature: bytes,
    trust_root: os.PathLike | str | None,
    payload_dir: os.PathLike | str,
    *,
    expected_publisher: str,
    expected_version: str | None = None,
    revoked_versions: frozenset[str] | set[str] = frozenset(),
    now: int | None = None,
) -> dict:
    """Verify signature, identity, validity window and the exact payload contents.

    Returns the parsed manifest. Raises ``ManifestError`` (or ``TrustRootNotConfigured``)
    on any mismatch; callers must not run or install payload files in that case.
    """
    doc, expected = _verify_header(
        manifest, signature, trust_root,
        expected_publisher=expected_publisher, expected_version=expected_version,
        revoked_versions=revoked_versions, now=now,
    )
    payload = Path(payload_dir)
    if not payload.is_dir():
        raise ManifestError("missing_payload", f"payload directory not found: {payload}")
    actual = _walk(payload)
    missing = sorted(set(expected) - actual)
    if missing:
        raise ManifestError("missing_file", ", ".join(missing))
    extra = sorted(actual - set(expected))
    if extra:
        raise ManifestError("unlisted_file", ", ".join(extra))
    for rel, (digest, size) in sorted(expected.items()):
        got_digest, got_size = _sha256(payload / rel)
        if got_size != size or got_digest != digest:
            raise ManifestError("digest_mismatch", rel)
    return doc


def verify_files(
    manifest: bytes,
    signature: bytes,
    trust_root: os.PathLike | str | None,
    files: dict[str, os.PathLike | str],
    *,
    expected_publisher: str,
    expected_version: str | None = None,
    revoked_versions: frozenset[str] | set[str] = frozenset(),
    now: int | None = None,
) -> dict:
    """Verify a subset of a release: every given file must be listed with matching size and SHA-256.

    ``files`` maps a manifest path to the local copy (for example an updater's staged files). A file the
    manifest does not list, or a symlink, is rejected; listed files that are not given are not checked.
    """
    doc, expected = _verify_header(
        manifest, signature, trust_root,
        expected_publisher=expected_publisher, expected_version=expected_version,
        revoked_versions=revoked_versions, now=now,
    )
    if not files:
        raise ManifestError("malformed_manifest", "no files were given to verify")
    for rel in sorted(files):
        _clean_path(rel)
        local = Path(files[rel])
        if rel not in expected:
            raise ManifestError("unlisted_file", rel)
        if local.is_symlink() or not local.is_file():
            raise ManifestError("missing_file", rel)
        digest, size = expected[rel]
        got_digest, got_size = _sha256(local)
        if got_size != size or got_digest != digest:
            raise ManifestError("digest_mismatch", rel)
    return doc


def _main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(description="Verify a signed ThingsPanel release manifest")
    parser.add_argument("--trust-root", required=False, help="pinned ECDSA P-256 public key (PEM)")
    parser.add_argument("--manifest", required=True)
    parser.add_argument("--signature", required=True)
    parser.add_argument("--payload", required=True)
    parser.add_argument("--publisher", required=True)
    parser.add_argument("--version")
    parser.add_argument("--revoked", action="append", default=[])
    args = parser.parse_args(argv)
    try:
        doc = verify_release(
            Path(args.manifest).read_bytes(),
            Path(args.signature).read_bytes(),
            args.trust_root,
            args.payload,
            expected_publisher=args.publisher,
            expected_version=args.version,
            revoked_versions=set(args.revoked),
        )
    except ManifestError as exc:
        print(f"release verification failed: {exc}", file=sys.stderr)
        return 2
    print(f"release {doc['release']['version']} verified ({len(doc['files'])} files)")
    return 0


if __name__ == "__main__":
    sys.exit(_main(sys.argv[1:]))
