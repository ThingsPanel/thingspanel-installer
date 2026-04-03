#!/bin/sh
# ThingsPanel All-in-One — bootstrap installer (POSIX sh)
#
# This wrapper exists so the recommended command works:
#   curl -fsSL https://install.thingspanel.io/install.sh | sh
#
# It downloads the real Bash installer and executes it with bash.
set -eu

RAW_BASE=${RAW_BASE:-https://install.thingspanel.io}
REAL_SCRIPT_URL="$RAW_BASE/install.bash"

if command -v bash >/dev/null 2>&1; then
  if command -v curl >/dev/null 2>&1; then
    exec curl -fsSL "$REAL_SCRIPT_URL" | bash
  elif command -v wget >/dev/null 2>&1; then
    exec wget -qO- "$REAL_SCRIPT_URL" | bash
  else
    echo "[ERROR] curl or wget is required to download installer." >&2
    exit 1
  fi
else
  echo "[ERROR] bash is required. Please install bash and re-run." >&2
  exit 1
fi
