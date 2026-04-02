#!/usr/bin/env bash
# ─────────────────────────────────────────────────────────────────────────────
# ThingsPanel All-in-One — 升级脚本
# 用法: ./upgrade.sh [版本号]
# 示例: ./upgrade.sh v1.2.0
# ─────────────────────────────────────────────────────────────────────────────
set -euo pipefail

INSTALL_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="ThingsPanel/all-in-one-assembler"
RAW_BASE="https://install.thingspanel.io"

RED='\033[0;31m'; YELLOW='\033[1;33m'; GREEN='\033[0;32m'
BLUE='\033[0;34m'; BOLD='\033[1m'; RESET='\033[0m'

info()    { echo -e "${BLUE}[INFO]${RESET}  $*"; }
success() { echo -e "${GREEN}[OK]${RESET}    $*"; }
warn()    { echo -e "${YELLOW}[WARN]${RESET}  $*"; }
error()   { echo -e "${RED}[ERROR]${RESET} $*" >&2; exit 1; }
step()    { echo -e "\n${BOLD}▶ $*${RESET}"; }

ENV_FILE="${INSTALL_DIR}/.env"

[ -f "$ENV_FILE" ] || error "未找到 .env 文件，请确认安装目录正确: $INSTALL_DIR"

main() {
    echo -e "${BOLD}ThingsPanel — 升级程序${RESET}"
    echo ""

    # ── 备份当前 .env ────────────────────────────────────────────────────────
    step "备份配置"
    cp "$ENV_FILE" "${ENV_FILE}.bak.$(date +%Y%m%d%H%M%S)"
    success ".env 已备份"

    # ── 确定目标版本 ──────────────────────────────────────────────────────────
    step "确定升级版本"
    TARGET_VERSION="${1:-}"
    if [ -z "$TARGET_VERSION" ]; then
        TARGET_VERSION=$(curl -fsSL \
            "https://api.github.com/repos/${REPO}/releases/latest" \
            2>/dev/null | grep '"tag_name"' | head -1 | sed 's/.*"tag_name": *"\([^"]*\)".*/\1/' || echo "")
        TARGET_VERSION="${TARGET_VERSION:-$(grep '^TP_VERSION=' "$ENV_FILE" | cut -d= -f2)}"
    fi

    CURRENT_VERSION="$(grep '^TP_VERSION=' "$ENV_FILE" | cut -d= -f2 || echo 'unknown')"
    info "当前版本: $CURRENT_VERSION"
    info "目标版本: $TARGET_VERSION"

    if [ "$CURRENT_VERSION" = "$TARGET_VERSION" ]; then
        warn "已是最新版本 $TARGET_VERSION，无需升级"
        exit 0
    fi

    # ── 更新版本号 ────────────────────────────────────────────────────────────
    step "更新版本配置"
    sed -i.bak \
        -e "s/^TP_VERSION=.*/TP_VERSION=${TARGET_VERSION}/" \
        -e "s/^TP_VUE_VERSION=.*/TP_VUE_VERSION=${TARGET_VERSION}/" \
        -e "s/^TP_BACKEND_VERSION=.*/TP_BACKEND_VERSION=${TARGET_VERSION}/" \
        "$ENV_FILE"
    success "版本号已更新为 $TARGET_VERSION"

    # ── 更新 compose 和 nginx 配置 ─────────────────────────────────────────────
    step "更新配置文件"
    curl -fsSL "${RAW_BASE}/docker-compose.yml" -o "${INSTALL_DIR}/docker-compose.yml"
    curl -fsSL "${RAW_BASE}/nginx/nginx.conf" -o "${INSTALL_DIR}/nginx/nginx.conf"
    success "配置文件已更新"

    # ── 拉取新镜像并重启 ──────────────────────────────────────────────────────
    step "升级服务"
    cd "$INSTALL_DIR"
    info "拉取新版本镜像..."
    docker compose pull --quiet
    info "重启服务（等待健康检查通过）..."
    docker compose up -d --wait --timeout 180
    success "升级完成！当前版本: $TARGET_VERSION"

    echo ""
    echo -e "${GREEN}✓ ThingsPanel 已成功升级到 ${BOLD}${TARGET_VERSION}${RESET}${GREEN}${RESET}"
    echo ""
}

main "$@"
