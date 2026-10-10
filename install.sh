#!/bin/sh
# ThingsPanel All-in-One — bootstrap installer (POSIX sh)
#
# This wrapper exists so the recommended command works:
#   curl -fsSL https://install.thingspanel.io/install.sh | sh
#
# It downloads the real Bash installer and executes it with bash.
set -eu

REPO="ThingsPanel/thingspanel-installer"
CHANNEL=${TP_INSTALLER_CHANNEL:-stable}

case "$CHANNEL" in
  stable|preview) ;;
  *) echo "[ERROR] TP_INSTALLER_CHANNEL must be stable or preview." >&2; exit 1 ;;
esac

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
LOCAL_CHECKOUT=0
if [ -f "$SCRIPT_DIR/install.bash" ] \
  && [ -f "$SCRIPT_DIR/docker-compose.yml" ] \
  && [ -f "$SCRIPT_DIR/scripts/notification-init.sh" ] \
  && [ -f "$SCRIPT_DIR/scripts/notification-migrate.sh" ] \
  && [ -f "$SCRIPT_DIR/init-db/00-timescale-extension.sh" ] \
  && [ -f "$SCRIPT_DIR/init-db/01-create-thingmodel-db.sh" ] \
  && [ -f "$SCRIPT_DIR/init-db/02-thingmodel-migrations.sh" ]; then
  LOCAL_CHECKOUT=1
fi

REF=${TP_INSTALLER_REF:-${TP_VERSION:-}}
if [ -n "${TP_INSTALLER_REF:-}" ] && [ -n "${TP_VERSION:-}" ] && [ "$TP_INSTALLER_REF" != "$TP_VERSION" ]; then
  echo "[ERROR] TP_VERSION and TP_INSTALLER_REF must identify the same installer resource ref." >&2
  exit 1
fi
if [ -z "$REF" ] && [ "$LOCAL_CHECKOUT" -eq 1 ] && command -v git >/dev/null 2>&1; then
  REF=$(git -C "$SCRIPT_DIR" rev-parse HEAD 2>/dev/null || true)
fi
if [ -z "$REF" ] && [ -f "$SCRIPT_DIR/.installer-bundle-ref" ]; then
  REF=$(head -n 1 "$SCRIPT_DIR/.installer-bundle-ref" | tr -d '\r\n')
fi
if [ -z "$REF" ]; then
  if [ "$CHANNEL" = "preview" ]; then
    echo "[ERROR] Preview installs require TP_INSTALLER_REF; stable assets will not be used." >&2
    exit 1
  fi
  if command -v curl >/dev/null 2>&1; then
    REF=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" \
      | sed -n 's/.*"tag_name":[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)
  elif command -v wget >/dev/null 2>&1; then
    REF=$(wget -qO- "https://api.github.com/repos/$REPO/releases/latest" \
      | sed -n 's/.*"tag_name":[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)
  fi
fi
case "$REF" in
  ""|-*|*..*|*[!A-Za-z0-9._/-]*) echo "[ERROR] A valid TP_INSTALLER_REF could not be resolved." >&2; exit 1 ;;
esac
export TP_INSTALLER_CHANNEL="$CHANNEL" TP_INSTALLER_REF="$REF"
URL="https://raw.githubusercontent.com/$REPO/$REF/install.bash"

if [ "$LOCAL_CHECKOUT" -eq 1 ]; then
  exec bash "$SCRIPT_DIR/install.bash" "$@"
fi

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

fetch_and_exec "$URL"
