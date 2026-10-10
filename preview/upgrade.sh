#!/usr/bin/env bash
set -euo pipefail
PREVIEW_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$PREVIEW_DIR"
test -f .preview-installed || { printf 'Use preview/start.sh for a fresh isolated installation.\n' >&2; exit 1; }
python3 verify-images.py --archives-only
for image_archive in images.d/*.tar.gz; do docker load -i "$image_archive"; done
python3 verify-images.py
export TP_INSTALLER_CHANNEL=preview
export TP_INSTALLER_REF="$(python3 -c 'import json; print(json.load(open("offline-images.lock.json"))["installerCommit"])')"
export TP_INSTALLER_LOCAL_ASSETS="$PREVIEW_DIR"
bash upgrade.sh
