#!/usr/bin/env bash
# ─────────────────────────────────────────────────────────────────────────────
# ThingsPanel All-in-One — Linux / macOS 安装脚本
#
# 用法（推荐）:
#   curl -fsSL https://install.thingspanel.io/install.sh | sh
#
# 本地运行:
#   chmod +x install.sh && ./install.sh
#
# 环境变量（可选）:
#   TP_VERSION      指定安装版本，默认最新
#   DATA_DIR        数据目录，默认 /opt/thingspanel/data
#   HTTP_PORT       Web 端口，默认 8080
#   MQTT_PORT       MQTT 端口，默认 1883
#   INSTALL_DIR     安装目录，默认 /opt/thingspanel
# ─────────────────────────────────────────────────────────────────────────────
set -eu

# ── 常量 ──────────────────────────────────────────────────────────────────────
REPO="ThingsPanel/all-in-one-assembler"
RAW_BASE="https://install.thingspanel.io"
INSTALL_DIR="${INSTALL_DIR:-/opt/thingspanel}"
DATA_DIR="${DATA_DIR:-${INSTALL_DIR}/data}"
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
    # 返回 0 表示 $1 >= $2
    printf '%s\n%s\n' "$2" "$1" | sort -V -C
}

gen_secret() {
    if command_exists openssl; then
        openssl rand -hex 32
    else
        cat /dev/urandom | tr -dc 'a-f0-9' | head -c 64
    fi
}

# ── Banner ────────────────────────────────────────────────────────────────────
print_banner() {
    echo -e "${BOLD}"
    echo " ████████╗██╗  ██╗██╗███╗   ██╗ ██████╗ ███████╗"
    echo "    ██╔══╝██║  ██║██║████╗  ██║██╔════╝ ██╔════╝"
    echo "    ██║   ███████║██║██╔██╗ ██║██║  ███╗███████╗"
    echo "    ██║   ██╔══██║██║██║╚██╗██║██║   ██║╚════██║"
    echo "    ██║   ██║  ██║██║██║ ╚████║╚██████╔╝███████║"
    echo "    ╚═╝   ╚═╝  ╚═╝╚═╝╚═╝  ╚═══╝ ╚═════╝ ╚══════╝"
    echo ""
    echo "             PANEL  All-in-One  Installer"
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

    # Docker Compose v2
    if docker compose version >/dev/null 2>&1; then
        COMPOSE_VER="$(docker compose version --short 2>/dev/null || echo '2.0')"
        success "Docker Compose v2 $COMPOSE_VER"
    else
        error "未找到 docker compose（v2）。请升级 Docker 或安装 Docker Compose Plugin：\n  https://docs.docker.com/compose/install/"
    fi
}

check_mirror() {
    step "选择镜像源"
    local aliyun_url="https://registry.cn-hangzhou.aliyuncs.com"
    local ghcr_url="https://ghcr.io"
    
    # Check GitHub Container Registry connection
    local ghcr_time=999
    local aliyun_time=999
    
    if command_exists curl; then
        aliyun_time=$(curl -o /dev/null -s -w "%{time_total}
" -m 3 $aliyun_url || echo 999)
        ghcr_time=$(curl -o /dev/null -s -w "%{time_total}
" -m 3 $ghcr_url || echo 999)
    fi
    
    # 简单的网速比较 (如果 aliyun_time < ghcr_time，或者GHCR超时，就选阿里云)
    if [ "$(echo "$aliyun_time < $ghcr_time" | bc -l 2>/dev/null || echo 1)" = "1" ]; then
        DOCKER_REGISTRY="registry.cn-hangzhou.aliyuncs.com"
        info "使用阿里云镜像源 (延迟: ${aliyun_time}s)"
    else
        DOCKER_REGISTRY="ghcr.io"
        info "使用 GHCR 镜像源 (延迟: ${ghcr_time}s)"
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
        # 从 GitHub API 获取最新版本
        if command_exists curl; then
            VERSION=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" 2>/dev/null \
                | grep '"tag_name"' | head -1 \
                | sed 's/.*"tag_name": *"\([^"]*\)".*/\1/' || true)
        fi
        VERSION="${VERSION:-v1.1.13.6}"
        if [ -z "$VERSION" ]; then
            VERSION="v1.1.13.6"
        fi
        info "最新版本: $VERSION"
    fi
    success "将安装版本: $VERSION"
}

# ── 创建目录结构 ───────────────────────────────────────────────────────────────
setup_directories() {
    step "创建目录结构"
    mkdir -p "$INSTALL_DIR"
    mkdir -p "$DATA_DIR"/{postgres,redis,gmqtt,backend/{files,configs}}
    success "目录: $INSTALL_DIR"
}

# ── 下载配置文件 ───────────────────────────────────────────────────────────────
download_configs() {
    step "下载配置文件"
    local compose_url="${RAW_BASE}/docker-compose.yml"
    local nginx_url="${RAW_BASE}/nginx/nginx.conf"

    if command_exists curl; then
        curl -fsSL "$compose_url" -o "${INSTALL_DIR}/docker-compose.yml"
        mkdir -p "${INSTALL_DIR}/nginx"
        curl -fsSL "$nginx_url" -o "${INSTALL_DIR}/nginx/nginx.conf"
    elif command_exists wget; then
        wget -qO "${INSTALL_DIR}/docker-compose.yml" "$compose_url"
        mkdir -p "${INSTALL_DIR}/nginx"
        wget -qO "${INSTALL_DIR}/nginx/nginx.conf" "$nginx_url"
    else
        error "未找到 curl 或 wget，无法下载配置文件"
    fi
    success "配置文件已下载到 $INSTALL_DIR"
}

# ── 生成 .env ──────────────────────────────────────────────────────────────────
generate_env() {
    step "生成环境变量"
    local env_file="${INSTALL_DIR}/.env"

    if [ -f "$env_file" ]; then
        warn ".env 已存在，跳过生成（如需重置密码请删除 $env_file 后重新运行）"
        return
    fi

    local pg_pass redis_pass auth_secret
    pg_pass=$(gen_secret)
    redis_pass=$(gen_secret)
    auth_secret=$(gen_secret)

    cat > "$env_file" << EOF
# ThingsPanel All-in-One — 自动生成于 $(date -u +"%Y-%m-%dT%H:%M:%SZ")
# 请妥善保管此文件，其中包含数据库密码等敏感信息

DOCKER_REGISTRY=${DOCKER_REGISTRY}
TP_VERSION=${VERSION}
TP_VUE_VERSION=${VERSION}
TP_BACKEND_VERSION=${VERSION}
TP_GMQTT_VERSION=v1.1.6
TP_REDIS_VERSION=6.2.7
TP_MODBUS_VERSION=v1.0.6.1
TP_HTTP_ADAPTER_VERSION=v1.0.0
TP_THINGSVIS_SERVER_VERSION=v1.0.4
TP_THINGSVIS_STUDIO_VERSION=v1.0.4
TP_TIMESCALEDB_VERSION=14

POSTGRES_PASSWORD=${pg_pass}
REDIS_PASSWORD=${redis_pass}
AUTH_SECRET=${auth_secret}

DATA_DIR=${DATA_DIR}
HTTP_PORT=${HTTP_PORT}
MQTT_PORT=${MQTT_PORT}
MODBUS_TCP_PORT=502
MODBUS_RTU_PORT=503

TZ=Asia/Shanghai
TP_LOG_LEVEL=error
EOF

    chmod 600 "$env_file"
    success ".env 已生成（权限 600，密码随机生成）"
}

# ── 初始化后端配置（避免空目录挂载覆盖镜像默认配置） ────────────────────────────
init_backend_configs() {
    local cfg_dir="${DATA_DIR}/backend/configs"
    local conf_file="${cfg_dir}/conf.yml"

    if [ -f "$conf_file" ]; then
        success "后端配置已存在：${conf_file}"
        return
    fi

    step "初始化后端默认配置"
    mkdir -p "$cfg_dir"

    local backend_image="${DOCKER_REGISTRY}/thingspanel/thingspanel-go:${VERSION}"
    info "从镜像提取默认配置: ${backend_image} → ${cfg_dir}"

    local cid
    cid=$(docker create "$backend_image" 2>/dev/null) || error "无法创建临时容器以提取默认配置（镜像可能拉取失败）：${backend_image}"

    if ! docker cp "${cid}:/go/src/app/configs/." "$cfg_dir" >/dev/null 2>&1; then
        docker rm "$cid" >/dev/null 2>&1 || true
        error "提取默认配置失败：docker cp ${cid}:/go/src/app/configs/. ${cfg_dir}"
    fi

    docker rm "$cid" >/dev/null 2>&1 || true

    if [ ! -f "$conf_file" ]; then
        error "初始化后未发现 conf.yml（期望：${conf_file}），请检查镜像内路径是否变化"
    fi

    success "后端默认配置已初始化"
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
        # 直接流式加载，避免拼接生成超大临时文件
        cat "$INSTALL_DIR"/images.tar.part-* | docker load || warn "镜像加载失败，将尝试在线拉取"
        success "离线镜像分片已加载"
    else
        # 拉取最新镜像
        info "拉取镜像（首次可能需要 3-5 分钟，取决于网速）..."
        docker compose pull --quiet
    fi

    init_backend_configs

    # 启动并等待所有 healthcheck 通过
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
    echo -e "  💾  数据目录:  ${DATA_DIR}"
    echo ""
    echo -e "${BOLD}常用命令:${RESET}"
    echo "  查看服务状态:  docker compose -f ${INSTALL_DIR}/docker-compose.yml ps"
    echo "  查看后端日志:  docker compose -f ${INSTALL_DIR}/docker-compose.yml logs -f backend"
    echo "  停止所有服务:  docker compose -f ${INSTALL_DIR}/docker-compose.yml down"
    echo "  升级到新版本:  ${INSTALL_DIR}/upgrade.sh"
    echo ""
}

# ── 下载管理脚本 ───────────────────────────────────────────────────────────────
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

# ── 主流程 ────────────────────────────────────────────────────────────────────
main() {
    print_banner
    check_os
    check_docker
    check_mirror
    check_ports
    check_memory
    resolve_version
    setup_directories
    download_configs
    generate_env
    download_management_scripts
    start_services
    verify_installation
    print_success
}

main "$@"
