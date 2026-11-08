#!/usr/bin/env python3
"""Build and sign the release manifest for a frozen installer payload.

Signing is the one step that needs the private key, so it lives outside the
managed payload. The manifest lists exactly the files the updater stages, and
is signed as DER ECDSA over SHA-256 of its exact bytes:

    python3 tools/sign_release.py --payload DIR --key release-key.pem \
        --version v1.3.0 --commit <40-hex> [--publisher URL] [--valid-days 30]

Writes DIR/release-manifest.json and DIR/release-manifest.json.sig. The matching
public key is the trust root that installers receive out of band.
"""
import argparse
import hashlib
import importlib.util
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import time

REPO = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location('tp_update_for_signing', REPO / 'config/native/update.py')
update = importlib.util.module_from_spec(spec)
spec.loader.exec_module(update)


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('--payload', required=True)
    parser.add_argument('--key', required=True, help='ECDSA P-256 private key (PEM)')
    parser.add_argument('--version', required=True)
    parser.add_argument('--commit', required=True, help='40-hex source commit of the payload')
    parser.add_argument('--publisher', default=update.DEFAULT_RELEASE_PUBLISHER)
    parser.add_argument('--valid-days', type=int, default=30)
    args = parser.parse_args()
    payload = Path(args.payload)
    with tempfile.TemporaryDirectory() as scratch:
        artifacts = [relative for relative, _ in update.Transaction(Path(scratch)).artifacts]
    files = []
    for relative in sorted(artifacts):
        source = payload / relative
        if source.is_symlink() or not source.is_file():
            sys.exit('payload is missing a managed file: ' + relative)
        data = source.read_bytes()
        files.append({'path': relative, 'sha256': hashlib.sha256(data).hexdigest(), 'size': len(data)})
    now = int(time.time())
    release = {'version': args.version, 'publisher': args.publisher, 'commit': args.commit,
               'not_before': now - 60, 'expires_at': now + args.valid_days * 86400}
    manifest = json.dumps({'schema': 'thingspanel.release-manifest.v1', 'release': release, 'files': files},
                          sort_keys=True).encode()
    (payload / update.RELEASE_MANIFEST).write_bytes(manifest)
    signature = subprocess.run(['openssl', 'dgst', '-sha256', '-sign', args.key], input=manifest,
                               stdout=subprocess.PIPE, check=True).stdout
    (payload / (update.RELEASE_MANIFEST + '.sig')).write_bytes(signature)
    print('signed %d files for %s' % (len(files), args.version))


if __name__ == '__main__':
    main()
