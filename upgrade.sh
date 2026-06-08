#!/usr/bin/env bash
# ─────────────────────────────────────────────────────────────────────────────
# ThingsPanel All-in-One — 升级脚本
# 用法: ./upgrade.sh [版本号]
# 示例: ./upgrade.sh v1.2.0
# ─────────────────────────────────────────────────────────────────────────────
set -euo pipefail

INSTALL_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="ThingsPanel/thingspanel-installer"
RAW_BASE="https://install.thingspanel.io"

RED='\033[0;31m'; YELLOW='\033[1;33m'; GREEN='\033[0;32m'
BLUE='\033[0;34m'; BOLD='\033[1m'; RESET='\033[0m'

info()    { echo -e "${BLUE}[INFO]${RESET}  $*"; }
success() { echo -e "${GREEN}[OK]${RESET}    $*"; }
warn()    { echo -e "${YELLOW}[WARN]${RESET}  $*"; }
error()   { echo -e "${RED}[ERROR]${RESET} $*" >&2; exit 1; }
step()    { echo -e "\n${BOLD}▶ $*${RESET}"; }

main() {
    echo -e "${BOLD}ThingsPanel — 升级程序${RESET}"
    echo ""

    TARGET_VERSION="${1:-}"
    if [ -z "$TARGET_VERSION" ]; then
        TARGET_VERSION=$(curl -fsSL \
            "https://api.github.com/repos/${REPO}/releases/latest" \
            2>/dev/null | grep '"tag_name"' | head -1 | sed 's/.*"tag_name": *"\([^"]*\)".*/\1/' || true)
    fi
    if [ -z "$TARGET_VERSION" ]; then
        error "无法获取最新版本，请手动指定：./upgrade.sh v1.2.0"
    fi
    info "目标版本: $TARGET_VERSION"

    step "备份配置文件"
    if [ -f "${INSTALL_DIR}/docker-compose.yml" ]; then
        cp "${INSTALL_DIR}/docker-compose.yml" "${INSTALL_DIR}/docker-compose.yml.bak.$(date +%Y%m%d%H%M%S)"
    fi
    success "已备份"

    step "更新配置文件"
    curl -fsSL "${RAW_BASE}/docker-compose.yml" -o "${INSTALL_DIR}/docker-compose.yml"
    success "配置文件已更新"

    step "拉取新镜像"
    cd "$INSTALL_DIR"
    docker compose pull --quiet
    success "镜像拉取完成"

    step "重启服务"
    docker compose up -d --wait --timeout 180
    success "升级完成！当前版本: $TARGET_VERSION"

    echo ""
    echo -e "${GREEN}✓ ThingsPanel 已成功升级到 ${BOLD}${TARGET_VERSION}${RESET}"
    echo ""
}

main "$@"
