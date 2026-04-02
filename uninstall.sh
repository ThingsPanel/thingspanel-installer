#!/usr/bin/env bash
# ─────────────────────────────────────────────────────────────────────────────
# ThingsPanel All-in-One — 卸载脚本
# 用法: ./uninstall.sh [--purge]
# --purge: 同时删除所有数据（postgres、redis 等），不可恢复！
# ─────────────────────────────────────────────────────────────────────────────
set -euo pipefail

INSTALL_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

RED='\033[0;31m'; YELLOW='\033[1;33m'; GREEN='\033[0;32m'
BLUE='\033[0;34m'; BOLD='\033[1m'; RESET='\033[0m'

info()    { echo -e "${BLUE}[INFO]${RESET}  $*"; }
success() { echo -e "${GREEN}[OK]${RESET}    $*"; }
warn()    { echo -e "${YELLOW}[WARN]${RESET}  $*"; }
error()   { echo -e "${RED}[ERROR]${RESET} $*" >&2; exit 1; }

PURGE=false
for arg in "$@"; do
    [ "$arg" = "--purge" ] && PURGE=true
done

echo -e "${BOLD}ThingsPanel — 卸载程序${RESET}"
echo ""

if $PURGE; then
    echo -e "${RED}${BOLD}警告：--purge 模式将删除所有数据，此操作不可恢复！${RESET}"
    read -r -p "请输入 'YES' 确认删除所有数据: " confirm
    [ "$confirm" = "YES" ] || { info "已取消"; exit 0; }
else
    echo -e "${YELLOW}将停止并移除所有 ThingsPanel 容器和镜像。${RESET}"
    echo "数据目录将保留。如需同时删除数据，请使用 --purge 参数"
    echo ""
    read -r -p "确认卸载？(y/N): " confirm
    [[ "$confirm" =~ ^[yY]$ ]] || { info "已取消"; exit 0; }
fi

echo ""
info "停止并移除容器..."
cd "$INSTALL_DIR"
docker compose down --remove-orphans --volumes=false 2>/dev/null || true
success "容器已移除"

info "移除 ThingsPanel 相关镜像..."
docker images --format '{{.Repository}}:{{.Tag}}' \
    | grep 'thingspanel\|thingsvis\|timescaledb' \
    | xargs -r docker rmi --force 2>/dev/null || true
success "镜像已移除"

if $PURGE; then
    DATA_DIR="$(grep '^DATA_DIR=' "${INSTALL_DIR}/.env" 2>/dev/null | cut -d= -f2 || echo "${INSTALL_DIR}/data")"
    info "删除数据目录: $DATA_DIR"
    rm -rf "$DATA_DIR"
    info "删除安装目录: $INSTALL_DIR"
    rm -rf "$INSTALL_DIR"
    success "所有数据已删除"
else
    warn "数据目录已保留，如需彻底清除请运行: $0 --purge"
fi

echo ""
echo -e "${GREEN}✓ ThingsPanel 已卸载${RESET}"
