#!/bin/sh
# ThingsPanel All-in-One — bootstrap installer (POSIX sh)
#
# This wrapper exists so the recommended command works:
#   curl -fsSL https://install.thingspanel.io/install.sh | sh
#
# It downloads the real Bash installer and executes it with bash.
set -eu

RAW_BASE=${RAW_BASE:-https://install.thingspanel.io}

# Primary: install.thingspanel.io (typically best for China via CDN)
# Fallback: GitHub raw (useful if custom domain is temporarily unreachable)
URL1="$RAW_BASE/install.bash"
URL2="https://raw.githubusercontent.com/ThingsPanel/thingspanel-installer/main/install.bash"

fetch_and_exec() {
  url="$1"
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$url" | bash
    return $?
  elif command -v wget >/dev/null 2>&1; then
    wget -qO- "$url" | bash
    return $?
  else
    echo "[ERROR] curl or wget is required to download installer." >&2
    return 1
  fi
}

if ! command -v bash >/dev/null 2>&1; then
  echo "[ERROR] bash is required. Please install bash and re-run." >&2
  exit 1
fi

fetch_and_exec "$URL1" || fetch_and_exec "$URL2"
