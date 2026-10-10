#!/usr/bin/env bash
set -euo pipefail
PREVIEW_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$PREVIEW_DIR"
# This script is also copied to the bundle root by the builder.
if [ ! -f offline-images.lock.json ]; then
    printf 'Run the start script inside the generated preview bundle.\n' >&2
    exit 1
fi
python3 verify-images.py --archives-only
for image_archive in images.d/*.tar.gz; do docker load -i "$image_archive"; done
python3 verify-images.py
export INSTALL_DIR="$PREVIEW_DIR"
export TP_INSTALLER_LOCAL_ASSETS="$PREVIEW_DIR"
export TP_INSTALLER_CHANNEL=preview
export TP_INSTALLER_REF="$(python3 -c 'import json; print(json.load(open("offline-images.lock.json"))["installerCommit"])')"
export HTTP_PORT=28080
bash install.bash
touch .preview-installed
