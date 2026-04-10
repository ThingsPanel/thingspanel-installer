#!/usr/bin/env bash
# ─────────────────────────────────────────────────────────────────────────────
# ThingsPanel All-in-One — Linux / macOS 安装脚本
#
# 用法（推荐）:
#   curl -fsSL https://install.thingspanel.io/install.sh | sh
#
# 本地运行:
#   chmod +x install.sh && ./install.sh
# ─────────────────────────────────────────────────────────────────────────────
set -eu

# ── 常量 ──────────────────────────────────────────────────────────────────────
REPO="ThingsPanel/thingspanel-installer"
RAW_BASE="https://install.thingspanel.io"
INSTALL_DIR="${INSTALL_DIR:-/opt/thingspanel}"
HTTP_PORT="${HTTP_PORT:-8080}"
MQTT_PORT="${MQTT_PORT:-1883}"
MIN_DOCKER_VERSION="20.10"
MIN_COMPOSE_VERSION="2.0"

# ── 颜色输出 ──────────────────────────────────────────────────────────────────
RED='\033[0;31m'; YELLOW='\033[1;33m'; GREEN='\033[0;32m'
BLUE='\033[0;34m'; BOLD='\033[1m'; RESET='\033[0m'

info()    { echo -e "${BLUE}[INFO]${RESET}  $*"; }
success() { echo -e "${GREEN}[OK]${RESET}    $*"; }
warn()    { echo -e "${YELLOW}[WARN]${RESET}  $*"; }
error()   { echo -e "${RED}[ERROR]${RESET} $*" >&2; exit 1; }
step()    { echo -e "\n${BOLD}▶ $*${RESET}"; }

# ── 工具函数 ──────────────────────────────────────────────────────────────────
command_exists() { command -v "$1" >/dev/null 2>&1; }

version_gte() {
    printf '%s\n%s\n' "$2" "$1" | sort -V -C
}

# ── Banner ────────────────────────────────────────────────────────────────────
print_banner() {
    echo -e "${BOLD}"
    echo ""
    echo "  ╔══════════════════════════════════════╗"
    echo "  ║         ThingsPanel                  ║"
    echo "  ║         Installer                    ║"
    echo "  ╚══════════════════════════════════════╝"
    echo ""
    echo -e "${RESET}"
}

# ── 环境检测 ──────────────────────────────────────────────────────────────────
check_os() {
    step "检测操作系统"
    OS="$(uname -s)"
    ARCH="$(uname -m)"
    case "$OS" in
        Linux)  OS_TYPE="linux" ;;
        Darwin) OS_TYPE="macos" ;;
        *)      error "不支持的操作系统: $OS（请在 Linux 或 macOS 上运行）" ;;
    esac
    success "操作系统: $OS ($ARCH)"
}

check_docker() {
    step "检测 Docker"
    if ! command_exists docker; then
        error "未找到 Docker。请先安装 Docker Engine（Linux）或 Docker Desktop（macOS）：\n  https://docs.docker.com/get-docker/"
    fi

    DOCKER_VER="$(docker version --format '{{.Server.Version}}' 2>/dev/null || echo '0.0')"
    if ! version_gte "$DOCKER_VER" "$MIN_DOCKER_VERSION"; then
        error "Docker 版本过低（当前: $DOCKER_VER，需要 >= $MIN_DOCKER_VERSION）"
    fi
    success "Docker $DOCKER_VER"

    if docker compose version >/dev/null 2>&1; then
        COMPOSE_VER="$(docker compose version --short 2>/dev/null || echo '2.0')"
        success "Docker Compose v2 $COMPOSE_VER"
    else
        error "未找到 docker compose（v2）。请升级 Docker 或安装 Docker Compose Plugin：\n  https://docs.docker.com/compose/install/"
    fi
}

check_ports() {
    step "检测端口占用"
    local ports=("$HTTP_PORT" "$MQTT_PORT")
    for port in "${ports[@]}"; do
        if command_exists ss; then
            if ss -tuln 2>/dev/null | grep -q ":${port} "; then
                warn "端口 ${port} 已被占用，可通过 HTTP_PORT= 或 MQTT_PORT= 指定其他端口"
            else
                success "端口 $port 可用"
            fi
        elif command_exists lsof; then
            if lsof -i ":${port}" >/dev/null 2>&1; then
                warn "端口 ${port} 已被占用"
            else
                success "端口 $port 可用"
            fi
        fi
    done
}

check_memory() {
    step "检测内存"
    local mem_kb=0
    if [ -f /proc/meminfo ]; then
        mem_kb=$(awk '/MemTotal/{print $2}' /proc/meminfo)
    elif command_exists sysctl; then
        mem_kb=$(( $(sysctl -n hw.memsize 2>/dev/null || echo 0) / 1024 ))
    fi

    local mem_gb=$(( mem_kb / 1024 / 1024 ))
    if [ "$mem_gb" -lt 2 ]; then
        warn "内存不足 2GB（当前约 ${mem_gb}GB），可能影响稳定性"
    else
        success "内存: 约 ${mem_gb}GB"
    fi
}

# ── 确定版本 ──────────────────────────────────────────────────────────────────
resolve_version() {
    step "确定安装版本"
    if [ -n "${TP_VERSION:-}" ]; then
        VERSION="$TP_VERSION"
        info "使用指定版本: $VERSION"
    else
        if command_exists curl; then
            VERSION=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" 2>/dev/null \
                | grep '"tag_name"' | head -1 \
                | sed 's/.*"tag_name": *"\([^"]*\)".*/\1/' || true)
        fi
        VERSION="${VERSION:-v1.1.14}"
        if [ -z "$VERSION" ]; then
            VERSION="v1.1.14"
        fi
        info "最新版本: $VERSION"
    fi
    success "将安装版本: $VERSION"
}

# ── 创建目录结构 ───────────────────────────────────────────────────────────────
setup_directories() {
    step "创建目录结构"
    mkdir -p "$INSTALL_DIR"
    success "目录: $INSTALL_DIR"
}

# ── 下载 docker-compose.yml ────────────────────────────────────────────────────
download_docker_compose() {
    step "下载 docker-compose.yml"
    local compose_url="${RAW_BASE}/docker-compose.yml"

    if command_exists curl; then
        curl -fsSL "$compose_url" -o "${INSTALL_DIR}/docker-compose.yml"
    elif command_exists wget; then
        wget -qO "${INSTALL_DIR}/docker-compose.yml" "$compose_url"
    else
        error "未找到 curl 或 wget，无法下载配置文件"
    fi
    success "配置文件已下载到 $INSTALL_DIR"
}

# ── 下载管理脚本 ────────────────────────────────────────────────────────────────
download_management_scripts() {
    step "下载管理脚本"
    for script in upgrade.sh uninstall.sh; do
        if command_exists curl; then
            curl -fsSL "${RAW_BASE}/${script}" -o "${INSTALL_DIR}/${script}"
        else
            wget -qO "${INSTALL_DIR}/${script}" "${RAW_BASE}/${script}"
        fi
        chmod +x "${INSTALL_DIR}/${script}"
    done
    success "管理脚本已就绪"
}

# ── 启动服务 ──────────────────────────────────────────────────────────────────
start_services() {
    step "启动 ThingsPanel 服务"
    cd "$INSTALL_DIR"

    if [ -f "$INSTALL_DIR/images.tar" ]; then
        info "发现本地离线镜像 images.tar，正在加载（这可能需要几分钟）..."
        docker load -i "$INSTALL_DIR/images.tar" || warn "镜像加载失败，将尝试在线拉取"
        success "离线镜像已加载"
    elif ls "$INSTALL_DIR"/images.tar.part-* >/dev/null 2>&1; then
        info "发现本地离线镜像分片 images.tar.part-*，正在加载（这可能需要几分钟）..."
        cat "$INSTALL_DIR"/images.tar.part-* | docker load || warn "镜像加载失败，将尝试在线拉取"
        success "离线镜像分片已加载"
    else
        info "拉取镜像（首次可能需要 3-5 分钟，取决于网速）..."
        docker compose pull --quiet
    fi

    info "启动服务，等待健康检查通过..."
    if ! docker compose up -d --wait --timeout 180 2>&1; then
        error "启动失败，请运行以下命令查看日志：\n  docker compose -f ${INSTALL_DIR}/docker-compose.yml logs"
    fi
    success "所有服务已启动"
}

# ── 验证安装 ──────────────────────────────────────────────────────────────────
verify_installation() {
    step "验证安装"
    local max_wait=60
    local waited=0

    info "等待 Web 服务就绪..."
    while [ "$waited" -lt "$max_wait" ]; do
        if command_exists curl; then
            if curl -sf "http://localhost:${HTTP_PORT}/health" >/dev/null 2>&1; then
                success "Web 服务已就绪 (http://localhost:${HTTP_PORT})"
                return
            fi
        elif command_exists wget; then
            if wget -qO- "http://localhost:${HTTP_PORT}/health" >/dev/null 2>&1; then
                success "Web 服务已就绪"
                return
            fi
        fi
        sleep 2
        waited=$(( waited + 2 ))
        echo -n "."
    done
    echo ""
    warn "Web 服务尚未响应，但容器已在后台运行。请稍后访问 http://localhost:${HTTP_PORT}"
}

# ── 安装完成提示 ───────────────────────────────────────────────────────────────
print_success() {
    echo ""
    echo -e "${GREEN}╔══════════════════════════════════════════════════════╗${RESET}"
    echo -e "${GREEN}║        ThingsPanel 安装成功！                        ║${RESET}"
    echo -e "${GREEN}╚══════════════════════════════════════════════════════╝${RESET}"
    echo ""
    echo -e "  🌐  Web 界面:   ${BOLD}http://localhost:${HTTP_PORT}${RESET}"
    echo -e "  📡  MQTT:       ${BOLD}localhost:${MQTT_PORT}${RESET}"
    echo ""
    echo -e "  📁  安装目录:  ${INSTALL_DIR}"
    echo ""
    echo -e "${BOLD}常用命令:${RESET}"
    echo "  查看服务状态:  docker compose -f ${INSTALL_DIR}/docker-compose.yml ps"
    echo "  查看后端日志:  docker compose -f ${INSTALL_DIR}/docker-compose.yml logs -f backend"
    echo "  停止所有服务:  docker compose -f ${INSTALL_DIR}/docker-compose.yml down"
    echo "  升级到新版本:  ${INSTALL_DIR}/upgrade.sh"
    echo ""
}

# ── 主流程 ─────────────────────────────────────────────────────���──────────────
main() {
    print_banner
    check_os
    check_docker
    check_ports
    check_memory
    resolve_version
    setup_directories
    download_docker_compose
    download_management_scripts
    start_services
    verify_installation
    print_success
}

main "$@"
