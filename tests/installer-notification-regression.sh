#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/tp-installer-test.XXXXXX")"
trap 'rm -rf "$TMP_ROOT"' EXIT

FAIL() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
ASSERT() { "$@" || FAIL "assertion failed: $*"; }

make_assets() {
    local root="$1"
    mkdir -p "$root/scripts" "$root/config" "$root/init-db"
    cat > "$root/docker-compose.yml" <<'EOF'
services:
  postgres: {}
  notification-setup: {}
  notification-migrate: {}
  backend: {}
EOF
    cat > "$root/scripts/notification-init.sh" <<'EOF'
#!/usr/bin/env sh
set -eu
printf 'init-wrapper\n' >> "$TEST_ACTION_LOG"
EOF
    cat > "$root/scripts/notification-init.ps1" <<'EOF'
throw 'fixture only'
EOF
    cat > "$root/scripts/notification-migrate.sh" <<'EOF'
#!/usr/bin/env sh
set -eu
printf 'migration-wrapper\n' >> "$TEST_ACTION_LOG"
EOF
    cat > "$root/scripts/notification-db-init.sh" <<'EOF'
#!/usr/bin/env sh
set -eu
printf 'database-wrapper\n' >> "$TEST_ACTION_LOG"
EOF
    cat > "$root/scripts/notification-nginx.sh" <<'EOF'
#!/usr/bin/env sh
set -eu
printf 'nginx-wrapper\n' >> "$TEST_ACTION_LOG"
EOF
    cat > "$root/scripts/notification-migrate.ps1" <<'EOF'
throw 'fixture only'
EOF
    cat > "$root/scripts/notification-db-init.ps1" <<'EOF'
throw 'fixture only'
EOF
    cat > "$root/scripts/notification-nginx.ps1" <<'EOF'
throw 'fixture only'
EOF
    printf '{"fixture":true}\n' > "$root/config/notification-runtime.json.template"
    printf 'fixture\n' > "$root/config/notification-caddyfile"
    printf 'fixture\n' > "$root/config/notification-routes.conf"
    printf '{"fixture":true}\n' > "$root/config/thingmodel-runtime.json"
    printf 'fixture\n' > "$root/init-db/notification-create-db.sql"
    printf '#!/bin/sh\n' > "$root/init-db/00-timescale-extension.sh"
    printf '#!/bin/sh\n' > "$root/init-db/01-create-thingmodel-db.sh"
    printf '#!/bin/sh\n' > "$root/init-db/02-thingmodel-migrations.sh"
    cp "$ROOT_DIR/uninstall.sh" "$root/uninstall.sh"
    cp "$ROOT_DIR/upgrade.sh" "$root/upgrade.sh"
    chmod +x "$root/scripts/notification-init.sh" "$root/scripts/notification-db-init.sh" "$root/scripts/notification-migrate.sh" "$root/scripts/notification-nginx.sh" "$root/upgrade.sh"
}

mkdir -p "$TMP_ROOT/bin"
cat > "$TMP_ROOT/bin/docker" <<'EOF'
#!/usr/bin/env sh
set -eu
printf 'docker %s\n' "$*" >> "$TEST_DOCKER_LOG"
if [ "${1:-}" = version ]; then echo 26.1.0; exit 0; fi
if [ "${1:-}" = compose ] && [ "${2:-}" = version ]; then echo 2.30.1; exit 0; fi
if [ "${1:-}" = load ] && [ -n "${TEST_DOCKER_LOAD_FAIL:-}" ]; then exit 17; fi
exit 0
EOF
cat > "$TMP_ROOT/bin/curl" <<'EOF'
#!/usr/bin/env sh
set -eu
url=""
out=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o) shift; out="$1" ;;
    http://*|https://*) url="$1" ;;
  esac
  shift
done
if [ -n "$out" ]; then
  printf 'download %s\n' "$url" >> "$TEST_CURL_LOG"
  relative="${url#*/preview-exact-ref/}"
  source="$TEST_REMOTE_ASSETS/$relative"
  [ -f "$source" ] || { printf 'missing fixture for %s\n' "$url" >&2; exit 22; }
  cp "$source" "$out"
  exit 0
fi
case "$url" in
  http://localhost:*/health) exit 0 ;;
  *) exit 22 ;;
esac
EOF
chmod +x "$TMP_ROOT/bin/docker" "$TMP_ROOT/bin/curl"
export PATH="$TMP_ROOT/bin:$PATH"
export TEST_DOCKER_LOG="$TMP_ROOT/docker.log" TEST_CURL_LOG="$TMP_ROOT/curl.log" TEST_ACTION_LOG="$TMP_ROOT/actions.log"

# Installer install flow against a complete local asset checkout and fake Docker.
LOCAL_ASSETS="$TMP_ROOT/local-assets"
make_assets "$LOCAL_ASSETS"
INSTALL_DIR="$TMP_ROOT/install" TP_INSTALLER_CHANNEL=preview TP_INSTALLER_REF=preview-exact-ref \
  TP_INSTALLER_LOCAL_ASSETS="$LOCAL_ASSETS" HTTP_PORT=18880 MQTT_PORT=18881 \
  bash "$ROOT_DIR/install.bash" > "$TMP_ROOT/install.out" 2>&1 || {
    cat "$TMP_ROOT/install.out" >&2
    FAIL "local install flow failed"
  }
ASSERT test -f "$TMP_ROOT/install/.installer-release"
ASSERT test -f "$TMP_ROOT/install/init-db/00-timescale-extension.sh"
ASSERT grep -q 'channel=preview' "$TMP_ROOT/install/.installer-release"
ASSERT grep -q 'ref=preview-exact-ref' "$TMP_ROOT/install/.installer-release"
ASSERT grep -q "source=local:$LOCAL_ASSETS" "$TMP_ROOT/install/.installer-release"
ASSERT grep -q 'docker compose run --rm --no-deps notification-setup' "$TEST_DOCKER_LOG"
ASSERT grep -q 'docker compose up -d postgres' "$TEST_DOCKER_LOG"
ASSERT grep -q 'docker compose up -d --wait' "$TEST_DOCKER_LOG"
ASSERT grep -q 'migration-wrapper' "$TEST_ACTION_LOG"
ASSERT grep -q 'database-wrapper' "$TEST_ACTION_LOG"
ASSERT grep -q 'nginx-wrapper' "$TEST_ACTION_LOG"

# A broken offline archive must stop instead of silently pulling another build.
OFFLINE_FAIL_DIR="$TMP_ROOT/offline-fail"
mkdir -p "$OFFLINE_FAIL_DIR/images.d"
printf 'broken shard fixture\n' > "$OFFLINE_FAIL_DIR/images.d/00-core.tar.gz"
: > "$TEST_DOCKER_LOG"
if INSTALL_DIR="$OFFLINE_FAIL_DIR" TP_INSTALLER_CHANNEL=preview TP_INSTALLER_REF=preview-exact-ref \
  TP_INSTALLER_LOCAL_ASSETS="$LOCAL_ASSETS" TEST_DOCKER_LOAD_FAIL=1 HTTP_PORT=18880 MQTT_PORT=18881 \
  bash "$ROOT_DIR/install.bash" > "$TMP_ROOT/offline-fail.out" 2>&1; then
    FAIL "invalid offline image archive unexpectedly succeeded"
fi
ASSERT grep -q '离线镜像分片加载失败' "$TMP_ROOT/offline-fail.out"
if grep -q 'docker compose pull' "$TEST_DOCKER_LOG"; then FAIL "invalid offline archive fell back to pulling images"; fi

# Upgrade fetches only the selected preview ref and preserves user files.
REMOTE_ASSETS="$TMP_ROOT/remote-assets"
make_assets "$REMOTE_ASSETS"
UPGRADE_DIR="$TMP_ROOT/upgrade"
mkdir -p "$UPGRADE_DIR/data/notification" "$UPGRADE_DIR/images.d"
printf 'offline image archive fixture\n' > "$UPGRADE_DIR/images.tar.gz"
printf 'offline shard 1\n' > "$UPGRADE_DIR/images.d/01-core.tar.gz"
printf 'offline shard 2\n' > "$UPGRADE_DIR/images.d/02-smtp.tar.gz"
cp "$ROOT_DIR/upgrade.sh" "$UPGRADE_DIR/upgrade.sh"
chmod +x "$UPGRADE_DIR/upgrade.sh"
printf 'CUSTOM=preserve-me\n' > "$UPGRADE_DIR/.env"
printf 'private-db-pass-sentinel\n' > "$UPGRADE_DIR/data/notification/db.pgpass"
: > "$TEST_DOCKER_LOG"
export TEST_REMOTE_ASSETS="$REMOTE_ASSETS"
TP_INSTALLER_CHANNEL=preview TP_INSTALLER_REF=preview-exact-ref \
  TEST_CURL_LOG="$TEST_CURL_LOG" TEST_DOCKER_LOG="$TEST_DOCKER_LOG" TEST_ACTION_LOG="$TEST_ACTION_LOG" \
  "$UPGRADE_DIR/upgrade.sh" preview-exact-ref > "$TMP_ROOT/upgrade.out" 2>&1 || {
    cat "$TMP_ROOT/upgrade.out" >&2
    FAIL "preview upgrade flow failed"
  }
ASSERT grep -q 'CUSTOM=preserve-me' "$UPGRADE_DIR/.env"
ASSERT grep -q 'private-db-pass-sentinel' "$UPGRADE_DIR/data/notification/db.pgpass"
ASSERT grep -q 'channel=preview' "$UPGRADE_DIR/.installer-release"
ASSERT grep -q 'ref=preview-exact-ref' "$UPGRADE_DIR/.installer-release"
ASSERT test -f "$UPGRADE_DIR/init-db/00-timescale-extension.sh"
ASSERT grep -q 'source=https://raw.githubusercontent.com/ThingsPanel/thingspanel-installer/preview-exact-ref' "$UPGRADE_DIR/.installer-release"
ASSERT grep -q 'https://raw.githubusercontent.com/ThingsPanel/thingspanel-installer/preview-exact-ref/' "$TEST_CURL_LOG"
ASSERT grep -q 'docker load -i .*images.d/01-core.tar.gz' "$TEST_DOCKER_LOG"
ASSERT grep -q 'docker load -i .*images.d/02-smtp.tar.gz' "$TEST_DOCKER_LOG"
if grep -q 'docker compose pull' "$TEST_DOCKER_LOG"; then FAIL "upgrade pulled images despite an offline archive"; fi
if grep -q 'install.thingspanel.io\|/main/' "$TEST_CURL_LOG"; then FAIL "preview request fell back to a stable resource"; fi
if grep -q 'docker compose down\|down --volumes\|down -v' "$TEST_DOCKER_LOG"; then FAIL "upgrade attempted a destructive compose down"; fi

# An installed preview keeps its saved channel/ref on an argument-free upgrade.
KEEP_DIR="$TMP_ROOT/upgrade-saved-preview"
mkdir -p "$KEEP_DIR"
cp "$ROOT_DIR/upgrade.sh" "$KEEP_DIR/upgrade.sh"
chmod +x "$KEEP_DIR/upgrade.sh"
printf 'channel=preview\nref=preview-exact-ref\n' > "$KEEP_DIR/.installer-release"
TP_INSTALLER_CHANNEL= TP_INSTALLER_REF= TEST_CURL_LOG="$TEST_CURL_LOG" \
  TEST_DOCKER_LOG="$TEST_DOCKER_LOG" TEST_ACTION_LOG="$TEST_ACTION_LOG" \
  "$KEEP_DIR/upgrade.sh" > "$TMP_ROOT/saved-preview.out" 2>&1 || {
    cat "$TMP_ROOT/saved-preview.out" >&2
    FAIL "saved preview upgrade flow failed"
  }

# A new preview install without a ref must fail closed before network or Docker.
mkdir -p "$TMP_ROOT/missing-ref"
cp "$ROOT_DIR/install.bash" "$TMP_ROOT/missing-ref/install.bash"
if TP_INSTALLER_CHANNEL=preview TP_INSTALLER_REF= bash "$TMP_ROOT/missing-ref/install.bash" \
    > "$TMP_ROOT/missing-ref.out" 2>&1; then
    FAIL "preview install without a ref unexpectedly succeeded"
fi
ASSERT grep -q 'preview 安装必须指定 TP_INSTALLER_REF' "$TMP_ROOT/missing-ref.out"

printf 'PASS: local install, pinned preview fetch/retention, offline image handling, and data preservation\n'
