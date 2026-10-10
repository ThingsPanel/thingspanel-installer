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
GITHUB_RAW_BASE="https://raw.githubusercontent.com/${REPO}"
INSTALL_DIR="${INSTALL_DIR:-/opt/thingspanel}"
HTTP_PORT="${HTTP_PORT:-8080}"
MQTT_PORT="${MQTT_PORT:-1883}"
MIN_DOCKER_VERSION="20.10"
MIN_COMPOSE_VERSION="2.30"
SUDO_CMD=""

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
    awk -v lhs="$1" -v rhs="$2" 'BEGIN {
        split(lhs, a, /[.+-]/); split(rhs, b, /[.+-]/)
        for (i = 1; i <= 3; i++) {
            av = a[i] + 0; bv = b[i] + 0
            if (av > bv) exit 0
            if (av < bv) exit 1
        }
        exit 0
    }'
}

run_privileged() {
    if [ -n "$SUDO_CMD" ]; then
        "$SUDO_CMD" "$@"
    else
        "$@"
    fi
}

ensure_install_dir_access() {
    step "检查安装目录权限"

    if [ "$(id -u)" -eq 0 ]; then
        success "当前用户可写入 $INSTALL_DIR"
        return
    fi

    if [ -d "$INSTALL_DIR" ] && [ -w "$INSTALL_DIR" ]; then
        success "当前用户可写入 $INSTALL_DIR"
        return
    fi

    local parent_dir
    parent_dir="$INSTALL_DIR"
    while [ ! -d "$parent_dir" ]; do
        local next_parent
        next_parent="$(dirname "$parent_dir")"
        if [ "$next_parent" = "$parent_dir" ]; then
            break
        fi
        parent_dir="$next_parent"
    done

    if [ ! -d "$INSTALL_DIR" ] && [ -d "$parent_dir" ] && [ -w "$parent_dir" ]; then
        success "当前用户可创建 $INSTALL_DIR"
        return
    fi

    if ! command_exists sudo; then
        error "当前用户没有权限写入 ${INSTALL_DIR}，且未找到 sudo。请使用有权限的用户运行，或设置 INSTALL_DIR 到可写目录。"
    fi

    SUDO_CMD="sudo"
    info "${INSTALL_DIR} 需要管理员权限，接下来会通过 sudo 创建和写入安装文件。"
    run_privileged -v || error "无法获取 sudo 权限，安装已停止"
    success "已获得安装目录写入权限"
}

download_file() {
    local url="$1"
    local target="$2"
    local mode="$3"
    local tmp_file

    tmp_file="$(mktemp "${TMPDIR:-/tmp}/thingspanel.XXXXXX")"

    if command_exists curl; then
        curl -fsSL "$url" -o "$tmp_file"
    elif command_exists wget; then
        wget -qO "$tmp_file" "$url"
    else
        rm -f "$tmp_file"
        error "未找到 curl 或 wget，无法下载安装文件"
    fi

    run_privileged mkdir -p "$(dirname "$target")"
    run_privileged install -m "$mode" "$tmp_file" "$target"
    rm -f "$tmp_file"
}

validate_installer_ref() {
    case "$1" in
        ""|-*|*..*|*[!A-Za-z0-9._/-]*) error "无效的 TP_INSTALLER_REF: $1" ;;
    esac
}

resolve_installer_source() {
    TP_INSTALLER_CHANNEL="${TP_INSTALLER_CHANNEL:-stable}"
    case "$TP_INSTALLER_CHANNEL" in
        stable|preview) ;;
        *) error "TP_INSTALLER_CHANNEL 仅支持 stable 或 preview" ;;
    esac

    if [ -n "${TP_INSTALLER_REF:-}" ] && [ -n "${VERSION:-}" ] && [ "$TP_INSTALLER_REF" != "$VERSION" ]; then
        error "TP_VERSION and TP_INSTALLER_REF must identify the same installer resource ref"
    fi
    local ref="${TP_INSTALLER_REF:-${VERSION:-}}"
    local bundle_ref_file="$(dirname "${BASH_SOURCE[0]}")/.installer-bundle-ref"
    if [ -z "$ref" ] && [ -f "$bundle_ref_file" ]; then
        ref="$(head -n 1 "$bundle_ref_file" | tr -d '\r\n')"
    fi
    if [ -z "$ref" ] && command_exists git; then
        ref="$(git -C "$(dirname "${BASH_SOURCE[0]}")" rev-parse HEAD 2>/dev/null || true)"
    fi
    if [ -z "$ref" ]; then
        if [ "$TP_INSTALLER_CHANNEL" = "preview" ]; then
            error "preview 安装必须指定 TP_INSTALLER_REF 或 TP_VERSION；不会回落到 stable 资源"
        fi
        error "无法确定安装资源版本"
    fi
    validate_installer_ref "$ref"
    TP_INSTALLER_REF="$ref"

    local script_dir=""
    if [ -n "${BASH_SOURCE[0]:-}" ]; then
        script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
    fi
    local local_assets="${TP_INSTALLER_LOCAL_ASSETS:-$script_dir}"
    if [ -n "$local_assets" ] && [ -f "$local_assets/docker-compose.yml" ] \
        && [ -f "$local_assets/scripts/notification-init.sh" ] \
        && [ -f "$local_assets/scripts/notification-db-init.sh" ] \
        && [ -f "$local_assets/scripts/notification-migrate.sh" ] \
        && [ -f "$local_assets/scripts/notification-nginx.sh" ] \
        && [ -f "$local_assets/config/notification-runtime.json.template" ] \
        && [ -f "$local_assets/config/notification-caddyfile" ] \
        && [ -f "$local_assets/config/notification-routes.conf" ] \
        && [ -f "$local_assets/config/thingmodel-runtime.json" ] \
        && [ -f "$local_assets/init-db/notification-create-db.sql" ] \
        && [ -f "$local_assets/init-db/00-timescale-extension.sh" ] \
        && [ -f "$local_assets/init-db/01-create-thingmodel-db.sh" ] \
        && [ -f "$local_assets/init-db/02-thingmodel-migrations.sh" ]; then
        ASSET_DIR="$local_assets"
        ASSET_BASE=""
        info "使用完整本地安装资源: $ASSET_DIR"
    else
        ASSET_DIR="${INSTALL_DIR}/.installer-assets/${TP_INSTALLER_REF}"
        ASSET_BASE="${GITHUB_RAW_BASE}/${TP_INSTALLER_REF}"
        info "使用 $TP_INSTALLER_CHANNEL 渠道资源 ref: $TP_INSTALLER_REF"
    fi
}

write_installer_marker() {
    local marker="${INSTALL_DIR}/.installer-release"
    local source_record
    if [ -n "$ASSET_BASE" ]; then source_record="${ASSET_BASE}"; else source_record="local:${ASSET_DIR}"; fi
    printf 'channel=%s\nref=%s\nsource=%s\n' "$TP_INSTALLER_CHANNEL" "$TP_INSTALLER_REF" "$source_record" \
        | run_privileged tee "$marker" >/dev/null
    run_privileged chmod 0644 "$marker"
}

stage_asset() {
    local relative="$1"
    local destination="$2"
    local mode="$3"
    if [ -n "$ASSET_BASE" ]; then
        download_file "${ASSET_BASE}/${relative}" "$destination" "$mode"
    else
        run_privileged mkdir -p "$(dirname "$destination")"
        local source="${ASSET_DIR}/${relative}"
        if [ -e "$destination" ] && [ "$source" -ef "$destination" ]; then
            run_privileged chmod "$mode" "$destination"
        else
            run_privileged install -m "$mode" "$source" "$destination"
        fi
    fi
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
        if ! version_gte "$COMPOSE_VER" "$MIN_COMPOSE_VERSION"; then
            error "Docker Compose 版本过低（当前: $COMPOSE_VER，需要 >= $MIN_COMPOSE_VERSION；请先升级 Compose）"
        fi
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
    elif [ -n "${TP_INSTALLER_REF:-}" ]; then
        VERSION="$TP_INSTALLER_REF"
        info "使用指定安装器 ref: $VERSION"
    elif [ -f "$(dirname "${BASH_SOURCE[0]}")/.installer-bundle-ref" ]; then
        VERSION="$(cat "$(dirname "${BASH_SOURCE[0]}")/.installer-bundle-ref")"
        info "使用安装包锁定 ref: $VERSION"
    elif command_exists git && git -C "$(dirname "${BASH_SOURCE[0]}")" rev-parse HEAD >/dev/null 2>&1; then
        VERSION="$(git -C "$(dirname "${BASH_SOURCE[0]}")" rev-parse HEAD)"
        info "使用本地 checkout ref: $VERSION"
    else
        if [ "${TP_INSTALLER_CHANNEL:-stable}" = "preview" ]; then
            error "preview 安装必须指定 TP_INSTALLER_REF；不会解析 stable release"
        fi
        if command_exists curl; then
            VERSION=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" 2>/dev/null \
                | grep '"tag_name"' | head -1 \
                | sed 's/.*"tag_name": *"\([^"]*\)".*/\1/' || true)
        fi
        VERSION="${VERSION:-v1.2.2}"
        if [ -z "$VERSION" ]; then
            VERSION="v1.2.2"
        fi
        info "最新版本: $VERSION"
    fi
    success "将安装版本: $VERSION"
    resolve_installer_source
}

# ── 创建目录结构 ───────────────────────────────────────────────────────────────
setup_directories() {
    step "创建目录结构"
    run_privileged mkdir -p "$INSTALL_DIR"
    success "目录: $INSTALL_DIR"
}

# ── 下载 docker-compose.yml ────────────────────────────────────────────────────
download_docker_compose() {
    step "下载 docker-compose.yml"
    stage_asset "docker-compose.yml" "${INSTALL_DIR}/docker-compose.yml" "0644"
    success "配置文件已下载到 $INSTALL_DIR"
}

download_notification_assets() {
    step "准备通知服务资产"
    stage_asset "scripts/notification-init.sh" "${INSTALL_DIR}/scripts/notification-init.sh" "0755"
    stage_asset "scripts/notification-db-init.sh" "${INSTALL_DIR}/scripts/notification-db-init.sh" "0755"
    stage_asset "scripts/notification-migrate.sh" "${INSTALL_DIR}/scripts/notification-migrate.sh" "0755"
    stage_asset "scripts/notification-nginx.sh" "${INSTALL_DIR}/scripts/notification-nginx.sh" "0755"
    stage_asset "config/notification-runtime.json.template" "${INSTALL_DIR}/config/notification-runtime.json.template" "0644"
    stage_asset "config/notification-caddyfile" "${INSTALL_DIR}/config/notification-caddyfile" "0644"
    stage_asset "config/notification-routes.conf" "${INSTALL_DIR}/config/notification-routes.conf" "0644"
    stage_asset "config/thingmodel-runtime.json" "${INSTALL_DIR}/config/thingmodel-runtime.json" "0644"
    stage_asset "init-db/notification-create-db.sql" "${INSTALL_DIR}/init-db/notification-create-db.sql" "0644"
    stage_asset "init-db/00-timescale-extension.sh" "${INSTALL_DIR}/init-db/00-timescale-extension.sh" "0755"
    stage_asset "init-db/01-create-thingmodel-db.sh" "${INSTALL_DIR}/init-db/01-create-thingmodel-db.sh" "0755"
    stage_asset "init-db/02-thingmodel-migrations.sh" "${INSTALL_DIR}/init-db/02-thingmodel-migrations.sh" "0755"
    success "通知服务资产已就绪"
}

# ── 下载管理脚本 ────────────────────────────────────────────────────────────────
download_management_scripts() {
    step "下载管理脚本"
    for script in upgrade.sh uninstall.sh; do
        stage_asset "$script" "${INSTALL_DIR}/${script}" "0755"
    done
    success "管理脚本已就绪"
}

# ── 启动服务 ──────────────────────────────────────────────────────────────────
start_services() {
    step "启动 ThingsPanel 服务"
    cd "$INSTALL_DIR"

    export TP_INSTALLER_UID="${TP_INSTALLER_UID:-${SUDO_UID:-$(id -u)}}"
    export TP_INSTALLER_GID="${TP_INSTALLER_GID:-${SUDO_GID:-$(id -g)}}"
    export TP_NOTIFICATION_DATA_DIR="${TP_NOTIFICATION_DATA_DIR:-${INSTALL_DIR}/data/notification}"
    installer_user_can_read() {
        local path="$1"
        if [ "$(id -u)" -eq 0 ] && [ "$TP_INSTALLER_UID" != "0" ]; then
            sudo -u "#${TP_INSTALLER_UID}" test -r "$path" 2>/dev/null
        else
            [ -r "$path" ]
        fi
    }
    installer_user_can_write() {
        local path="$1"
        if [ "$(id -u)" -eq 0 ] && [ "$TP_INSTALLER_UID" != "0" ]; then
            sudo -u "#${TP_INSTALLER_UID}" test -w "$path" 2>/dev/null
        else
            [ -w "$path" ]
        fi
    }
    if [ ! -e "$TP_NOTIFICATION_DATA_DIR" ]; then
        run_privileged mkdir -p "$TP_NOTIFICATION_DATA_DIR"
        run_privileged chown "$TP_INSTALLER_UID:$TP_INSTALLER_GID" "$TP_NOTIFICATION_DATA_DIR"
        run_privileged chmod 0700 "$TP_NOTIFICATION_DATA_DIR"
    elif ! installer_user_can_read "$TP_NOTIFICATION_DATA_DIR" || ! installer_user_can_write "$TP_NOTIFICATION_DATA_DIR"; then
        error "通知数据目录不可由当前用户读取和写入；请由现有文件所有者或管理员运行升级，不会自动放宽密钥权限"
    fi
    for private_file in infra.json core.env db.env smtp.env migrate.env legacy-reader.env db.pgpass tls/ca.crt tls/server.crt tls/server.key tls/system-ca-bundle.pem; do
        if [ -e "$TP_NOTIFICATION_DATA_DIR/$private_file" ] && ! installer_user_can_read "$TP_NOTIFICATION_DATA_DIR/$private_file"; then
            error "现有通知凭据不可读: $TP_NOTIFICATION_DATA_DIR/$private_file；请由文件所有者或管理员运行，不会自动放宽权限"
        fi
    done

    if [ -d "$INSTALL_DIR/images.d" ]; then
        local found_image_shard=0
        for image_shard in "$INSTALL_DIR"/images.d/*.tar.gz; do
            [ -f "$image_shard" ] || continue
            found_image_shard=1
            info "导入离线镜像分片: $(basename "$image_shard")"
            docker load -i "$image_shard" || error "离线镜像分片加载失败: ${image_shard}；已停止且不会回退拉取镜像"
        done
        [ "$found_image_shard" -eq 1 ] || error "images.d 存在但没有 .tar.gz 镜像分片；安装已停止且不会在线回退"
        success "离线镜像分片已加载"
    elif [ -f "$INSTALL_DIR/images.tar.gz" ]; then
        info "发现本地离线镜像 images.tar.gz，正在直接导入（这可能需要几分钟）..."
        docker load -i "$INSTALL_DIR/images.tar.gz" || error "离线镜像包加载失败，已停止以避免改用其他镜像"
        success "离线镜像已加载"
    elif [ -f "$INSTALL_DIR/images.tar" ]; then
        info "发现本地离线镜像 images.tar，正在加载（这可能需要几分钟）..."
        docker load -i "$INSTALL_DIR/images.tar" || error "离线镜像包加载失败，已停止以避免改用其他镜像"
        success "离线镜像已加载"
    elif ls "$INSTALL_DIR"/images.tar.part-* >/dev/null 2>&1; then
        info "发现本地离线镜像分片 images.tar.part-*，正在加载（这可能需要几分钟）..."
        cat "$INSTALL_DIR"/images.tar.part-* | docker load || error "离线镜像分片加载失败，已停止以避免改用其他镜像"
        success "离线镜像分片已加载"
    else
        info "拉取镜像（首次可能需要 3-5 分钟，取决于网速）..."
        docker compose pull --quiet
    fi

    step "初始化通知服务私密配置（仅补齐缺失文件）"
    docker compose run --rm --no-deps notification-setup

    step "启动数据库并执行通知数据库迁移"
    docker compose up -d postgres
    docker compose exec -T postgres sh -c 'until pg_isready -U "$POSTGRES_USER" -d "$POSTGRES_DB"; do sleep 2; done'
    "${INSTALL_DIR}/scripts/notification-db-init.sh"
    "${INSTALL_DIR}/scripts/notification-migrate.sh"

    info "启动服务，等待健康检查通过..."
    if ! docker compose up -d --wait --timeout 180 2>&1; then
        error "启动失败，请运行以下命令查看日志：\n  docker compose -f ${INSTALL_DIR}/docker-compose.yml logs"
    fi
    "${INSTALL_DIR}/scripts/notification-nginx.sh"
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
    ensure_install_dir_access
    setup_directories
    download_docker_compose
    download_notification_assets
    download_management_scripts
    start_services
    verify_installation
    write_installer_marker
    print_success
}

main "$@"
