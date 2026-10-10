#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"
VERSION="${TP_VERSION:-v1.2.2}"
DIST_DIR="$ROOT_DIR/dist/macos"
BUILD_DIR="$SCRIPT_DIR/.build"
PAYLOAD_DIR="$BUILD_DIR/payload"
COMPONENT_PKG="$BUILD_DIR/thingspanel-component.pkg"
FINAL_PKG="$DIST_DIR/ThingsPanel-${VERSION}.pkg"

rm -rf "$BUILD_DIR"
mkdir -p "$PAYLOAD_DIR/opt/thingspanel" "$DIST_DIR"
mkdir -p "$PAYLOAD_DIR/opt/thingspanel/scripts" "$PAYLOAD_DIR/opt/thingspanel/config" "$PAYLOAD_DIR/opt/thingspanel/init-db"

cp "$ROOT_DIR/docker-compose.yml" "$PAYLOAD_DIR/opt/thingspanel/"
printf '%s\n' "$VERSION" > "$PAYLOAD_DIR/opt/thingspanel/.installer-bundle-ref"
cp "$ROOT_DIR/scripts/notification-init.sh" "$PAYLOAD_DIR/opt/thingspanel/scripts/"
cp "$ROOT_DIR/scripts/notification-db-init.sh" "$PAYLOAD_DIR/opt/thingspanel/scripts/"
cp "$ROOT_DIR/scripts/notification-migrate.sh" "$PAYLOAD_DIR/opt/thingspanel/scripts/"
cp "$ROOT_DIR/scripts/notification-nginx.sh" "$PAYLOAD_DIR/opt/thingspanel/scripts/"
cp "$ROOT_DIR/config/notification-runtime.json.template" "$PAYLOAD_DIR/opt/thingspanel/config/"
cp "$ROOT_DIR/config/notification-caddyfile" "$ROOT_DIR/config/notification-routes.conf" "$ROOT_DIR/config/thingmodel-runtime.json" "$PAYLOAD_DIR/opt/thingspanel/config/"
cp "$ROOT_DIR/init-db/00-timescale-extension.sh" "$ROOT_DIR/init-db/notification-create-db.sql" "$ROOT_DIR/init-db/01-create-thingmodel-db.sh" "$ROOT_DIR/init-db/02-thingmodel-migrations.sh" "$PAYLOAD_DIR/opt/thingspanel/init-db/"
cp "$ROOT_DIR/install.sh" "$PAYLOAD_DIR/opt/thingspanel/"
cp "$ROOT_DIR/install.bash" "$PAYLOAD_DIR/opt/thingspanel/"
cp "$ROOT_DIR/upgrade.sh" "$PAYLOAD_DIR/opt/thingspanel/"
cp "$ROOT_DIR/uninstall.sh" "$PAYLOAD_DIR/opt/thingspanel/"
chmod +x "$PAYLOAD_DIR/opt/thingspanel/"*.sh "$PAYLOAD_DIR/opt/thingspanel/scripts/"*.sh "$SCRIPT_DIR/scripts/"*
chmod +x "$PAYLOAD_DIR/opt/thingspanel/init-db/"*.sh

pkgbuild \
  --root "$PAYLOAD_DIR" \
  --identifier "io.thingspanel.installer" \
  --version "${VERSION#v}" \
  --scripts "$SCRIPT_DIR/scripts" \
  --install-location "/" \
  "$COMPONENT_PKG"

productbuild \
  --distribution "$SCRIPT_DIR/Distribution.xml" \
  --package-path "$BUILD_DIR" \
  --resources "$SCRIPT_DIR/resources" \
  "$FINAL_PKG"

rm -rf "$BUILD_DIR"
echo "Built: $FINAL_PKG"
