#!/usr/bin/env bash
# ─────────────────────────────────────────────────────────────────────────────
# ThingsPanel All-in-One — macOS .pkg 安装包构建脚本
#
# 前置条件:
#   - macOS 系统（需要 pkgbuild 和 productbuild 命令）
#   - 已设置 TP_VERSION 环境变量（或使用默认值）
#
# 使用:
#   cd packaging/macos && ./build.sh
#   输出: ../../dist/macos/ThingsPanel-${VERSION}.pkg
# ─────────────────────────────────────────────────────────────────────────────
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"
VERSION="${TP_VERSION:-v1.1.13.6}"
DIST_DIR="$ROOT_DIR/dist/macos"
BUILD_DIR="$SCRIPT_DIR/.build"
PAYLOAD_DIR="$BUILD_DIR/payload"
COMPONENT_PKG="$BUILD_DIR/thingspanel-component.pkg"
FINAL_PKG="$DIST_DIR/ThingsPanel-${VERSION}.pkg"

echo "────────────────────────────────────────────"
echo "  构建 ThingsPanel macOS 安装包"
echo "  版本: $VERSION"
echo "  输出: $FINAL_PKG"
echo "────────────────────────────────────────────"

# ── 准备目录 ──────────────────────────────────────────────────────────────────
rm -rf "$BUILD_DIR"
mkdir -p "$PAYLOAD_DIR/opt/thingspanel"
mkdir -p "$DIST_DIR"

# ── 拷贝安装文件到 payload ────────────────────────────────────────────────────
echo "[1/4] 准备安装文件..."
cp "$ROOT_DIR/docker-compose.yml"   "$PAYLOAD_DIR/opt/thingspanel/"
cp "$ROOT_DIR/install.sh"            "$PAYLOAD_DIR/opt/thingspanel/"
cp "$ROOT_DIR/upgrade.sh"            "$PAYLOAD_DIR/opt/thingspanel/"
cp "$ROOT_DIR/uninstall.sh"          "$PAYLOAD_DIR/opt/thingspanel/"
mkdir -p "$PAYLOAD_DIR/opt/thingspanel/nginx"
cp "$ROOT_DIR/nginx/nginx.conf"      "$PAYLOAD_DIR/opt/thingspanel/nginx/"
chmod +x "$PAYLOAD_DIR/opt/thingspanel/"*.sh

# ── 构建组件包 ────────────────────────────────────────────────────────────────
echo "[2/4] 构建组件包..."
pkgbuild \
    --root "$PAYLOAD_DIR" \
    --identifier "io.thingspanel.aio" \
    --version "${VERSION#v}" \
    --scripts "$SCRIPT_DIR/scripts" \
    --install-location "/" \
    "$COMPONENT_PKG"

# ── 构建产品包（带向导界面）──────────────────────────────────────────────────
echo "[3/4] 构建产品安装包..."
productbuild \
    --distribution "$SCRIPT_DIR/Distribution.xml" \
    --package-path "$BUILD_DIR" \
    --resources "$SCRIPT_DIR/resources" \
    "$FINAL_PKG"

# ── 清理临时文件 ──────────────────────────────────────────────────────────────
echo "[4/4] 清理..."
rm -rf "$BUILD_DIR"

echo ""
echo "✓ 构建完成: $FINAL_PKG"
echo "  大小: $(du -sh "$FINAL_PKG" | cut -f1)"

# 可选：代码签名（需要 Apple Developer 证书）
if [ -n "${APPLE_DEVELOPER_ID:-}" ]; then
    echo "  签名安装包..."
    productsign \
        --sign "$APPLE_DEVELOPER_ID" \
        "$FINAL_PKG" \
        "${FINAL_PKG/.pkg/-signed.pkg}"
    mv "${FINAL_PKG/.pkg/-signed.pkg}" "$FINAL_PKG"
    echo "  ✓ 签名完成"
fi
