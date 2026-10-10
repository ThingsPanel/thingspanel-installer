#!/usr/bin/env bash
# ─────────────────────────────────────────────────────────────────────────────
# ThingsPanel All-in-One — 升级脚本
# 用法: ./upgrade.sh [版本号]
# 示例: ./upgrade.sh v1.2.2
# ─────────────────────────────────────────────────────────────────────────────
set -euo pipefail

INSTALL_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="ThingsPanel/thingspanel-installer"
GITHUB_RAW_BASE="https://raw.githubusercontent.com/${REPO}"
MIN_COMPOSE_VERSION="2.30"

RED='\033[0;31m'; YELLOW='\033[1;33m'; GREEN='\033[0;32m'
BLUE='\033[0;34m'; BOLD='\033[1m'; RESET='\033[0m'

info()    { echo -e "${BLUE}[INFO]${RESET}  $*"; }
success() { echo -e "${GREEN}[OK]${RESET}    $*"; }
warn()    { echo -e "${YELLOW}[WARN]${RESET}  $*"; }
error()   { echo -e "${RED}[ERROR]${RESET} $*" >&2; exit 1; }
step()    { echo -e "\n${BOLD}▶ $*${RESET}"; }
run_privileged() {
    if [ "$(id -u)" -eq 0 ]; then "$@"; else sudo "$@"; fi
}
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

validate_installer_ref() {
    case "$1" in
        ""|-*|*..*|*[!A-Za-z0-9._/-]*) error "Invalid TP_INSTALLER_REF: $1" ;;
    esac
}

resolve_installer_source() {
    local ref="${TP_INSTALLER_REF:-$TARGET_VERSION}"
    TP_INSTALLER_CHANNEL="${TP_INSTALLER_CHANNEL:-stable}"
    case "$TP_INSTALLER_CHANNEL" in
        stable|preview) ;;
        *) error "TP_INSTALLER_CHANNEL must be stable or preview" ;;
    esac
    if [ -z "$ref" ]; then
        error "Preview upgrades require TP_INSTALLER_REF or an explicit target version; stable resources are never used as fallback"
    fi
    validate_installer_ref "$ref"
    TP_INSTALLER_REF="$ref"
    local checkout_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
    local local_assets="${TP_INSTALLER_LOCAL_ASSETS:-}"
    if [ -z "$local_assets" ] && [ -e "$checkout_dir/.git" ]; then
        local_assets="$checkout_dir"
    elif [ -z "$local_assets" ] && [ -n "${SAVED_INSTALLER_REF:-}" ] \
        && [ "$SAVED_INSTALLER_REF" = "$TP_INSTALLER_REF" ]; then
        local_assets="$checkout_dir"
    fi
    if [ -f "$local_assets/docker-compose.yml" ] \
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
        info "Using complete local installer assets: $ASSET_DIR"
    else
        ASSET_DIR="${INSTALL_DIR}/.installer-assets/${TP_INSTALLER_REF}"
        ASSET_BASE="${GITHUB_RAW_BASE}/${TP_INSTALLER_REF}"
        info "Using $TP_INSTALLER_CHANNEL channel ref: $TP_INSTALLER_REF"
    fi
}

stage_asset() {
    local relative="$1" destination="$2" mode="$3"
    if [ -n "$ASSET_BASE" ]; then
        local tmp_file
        tmp_file="$(mktemp "${TMPDIR:-/tmp}/thingspanel.XXXXXX")"
        if command -v curl >/dev/null 2>&1; then
            curl -fsSL "${ASSET_BASE}/${relative}" -o "$tmp_file"
        elif command -v wget >/dev/null 2>&1; then
            wget -qO "$tmp_file" "${ASSET_BASE}/${relative}"
        else
            rm -f "$tmp_file"
            error "curl or wget is required to download installer assets"
        fi
        mkdir -p "$(dirname "$destination")"
        install -m "$mode" "$tmp_file" "$destination"
        rm -f "$tmp_file"
    else
        mkdir -p "$(dirname "$destination")"
        local source="${ASSET_DIR}/${relative}"
        if [ -e "$destination" ] && [ "$source" -ef "$destination" ]; then
            chmod "$mode" "$destination"
        else
            install -m "$mode" "$source" "$destination"
        fi
    fi
}

main() {
    echo -e "${BOLD}ThingsPanel — 升级程序${RESET}"
    echo ""

    local marker="${INSTALL_DIR}/.installer-release"
    local saved_channel="" saved_ref=""
    if [ -f "$marker" ]; then
        saved_channel="$(sed -n 's/^channel=//p' "$marker" | head -1)"
        saved_ref="$(sed -n 's/^ref=//p' "$marker" | head -1)"
    fi
    SAVED_INSTALLER_REF="$saved_ref"
    TP_INSTALLER_CHANNEL="${TP_INSTALLER_CHANNEL:-${saved_channel:-stable}}"
    TARGET_VERSION="${1:-${TP_INSTALLER_REF:-}}"
    if [ -z "$TARGET_VERSION" ] && [ "$TP_INSTALLER_CHANNEL" = "preview" ]; then
        TARGET_VERSION="$saved_ref"
    fi
    if [ -z "$TARGET_VERSION" ]; then
        if [ "$TP_INSTALLER_CHANNEL" = "preview" ]; then
            error "Preview upgrades need the saved ref, TP_INSTALLER_REF, or an explicit target version"
        fi
        TARGET_VERSION=$(curl -fsSL \
            "https://api.github.com/repos/${REPO}/releases/latest" \
            2>/dev/null | grep '"tag_name"' | head -1 | sed 's/.*"tag_name": *"\([^"]*\)".*/\1/' || true)
    fi
    if [ -z "$TARGET_VERSION" ]; then
        error "无法获取最新版本，请手动指定：./upgrade.sh v1.2.2"
    fi
    info "目标版本: $TARGET_VERSION"
    if [ -n "${TP_INSTALLER_REF:-}" ] && [ -n "${1:-}" ] && [ "$TP_INSTALLER_REF" != "$1" ]; then
        error "TP_INSTALLER_REF and explicit target version differ; choose one exact installer ref"
    fi
    resolve_installer_source

    if ! command -v docker >/dev/null 2>&1; then
        error "未找到 Docker；升级已停止"
    fi
    local compose_version
    compose_version="$(docker compose version --short 2>/dev/null || true)"
    if [ -z "$compose_version" ] || ! version_gte "$compose_version" "$MIN_COMPOSE_VERSION"; then
        error "Docker Compose 版本过低或不可用（需要 >= $MIN_COMPOSE_VERSION）；升级已在修改文件前停止"
    fi

    step "备份配置文件"
    if [ -f "${INSTALL_DIR}/docker-compose.yml" ]; then
        cp "${INSTALL_DIR}/docker-compose.yml" "${INSTALL_DIR}/docker-compose.yml.bak.$(date +%Y%m%d%H%M%S)"
    fi
    success "已备份"

    step "更新配置文件"
    stage_asset "docker-compose.yml" "${INSTALL_DIR}/docker-compose.yml" "0644"
    stage_asset "scripts/notification-init.sh" "${INSTALL_DIR}/scripts/notification-init.sh" "0755"
    stage_asset "scripts/notification-db-init.sh" "${INSTALL_DIR}/scripts/notification-db-init.sh" "0755"
    stage_asset "scripts/notification-migrate.sh" "${INSTALL_DIR}/scripts/notification-migrate.sh" "0755"
    stage_asset "scripts/notification-nginx.sh" "${INSTALL_DIR}/scripts/notification-nginx.sh" "0755"
    stage_asset "scripts/notification-migrate.ps1" "${INSTALL_DIR}/scripts/notification-migrate.ps1" "0644"
    stage_asset "scripts/notification-db-init.ps1" "${INSTALL_DIR}/scripts/notification-db-init.ps1" "0644"
    stage_asset "scripts/notification-nginx.ps1" "${INSTALL_DIR}/scripts/notification-nginx.ps1" "0644"
    stage_asset "config/notification-runtime.json.template" "${INSTALL_DIR}/config/notification-runtime.json.template" "0644"
    stage_asset "config/notification-caddyfile" "${INSTALL_DIR}/config/notification-caddyfile" "0644"
    stage_asset "config/notification-routes.conf" "${INSTALL_DIR}/config/notification-routes.conf" "0644"
    stage_asset "config/thingmodel-runtime.json" "${INSTALL_DIR}/config/thingmodel-runtime.json" "0644"
    stage_asset "init-db/notification-create-db.sql" "${INSTALL_DIR}/init-db/notification-create-db.sql" "0644"
    stage_asset "init-db/00-timescale-extension.sh" "${INSTALL_DIR}/init-db/00-timescale-extension.sh" "0755"
    stage_asset "init-db/01-create-thingmodel-db.sh" "${INSTALL_DIR}/init-db/01-create-thingmodel-db.sh" "0755"
    stage_asset "init-db/02-thingmodel-migrations.sh" "${INSTALL_DIR}/init-db/02-thingmodel-migrations.sh" "0755"
    stage_asset "upgrade.sh" "${INSTALL_DIR}/upgrade.sh" "0755"
    stage_asset "uninstall.sh" "${INSTALL_DIR}/uninstall.sh" "0755"
    success "配置文件已更新"

    step "拉取新镜像"
    cd "$INSTALL_DIR"
    if [ -d "$INSTALL_DIR/images.d" ]; then
        local found_image_shard=0
        for image_shard in "$INSTALL_DIR"/images.d/*.tar.gz; do
            [ -f "$image_shard" ] || continue
            found_image_shard=1
            info "导入离线镜像分片: $(basename "$image_shard")"
            docker load -i "$image_shard" || error "离线镜像分片加载失败: ${image_shard}；升级已停止且不会回退拉取镜像"
        done
        [ "$found_image_shard" -eq 1 ] || error "images.d 存在但没有 .tar.gz 镜像分片；升级已停止且不会在线回退"
        success "离线镜像分片已加载"
    elif [ -f "$INSTALL_DIR/images.tar.gz" ]; then
        docker load -i "$INSTALL_DIR/images.tar.gz" || error "离线镜像包加载失败，升级已停止且不会回退拉取镜像"
        success "离线镜像已加载"
    elif [ -f "$INSTALL_DIR/images.tar" ]; then
        docker load -i "$INSTALL_DIR/images.tar" || error "离线镜像包加载失败，升级已停止且不会回退拉取镜像"
        success "离线镜像已加载"
    elif ls "$INSTALL_DIR"/images.tar.part-* >/dev/null 2>&1; then
        cat "$INSTALL_DIR"/images.tar.part-* | docker load || error "离线镜像分片加载失败，升级已停止且不会回退拉取镜像"
        success "离线镜像分片已加载"
    else
        docker compose pull --quiet || error "镜像拉取失败，升级已停止"
        success "镜像拉取完成"
    fi

    step "补齐通知私密配置并执行可恢复迁移"
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
        if mkdir -p "$TP_NOTIFICATION_DATA_DIR" 2>/dev/null; then
            chmod 0700 "$TP_NOTIFICATION_DATA_DIR"
        else
            run_privileged mkdir -p "$TP_NOTIFICATION_DATA_DIR"
            run_privileged chown "$TP_INSTALLER_UID:$TP_INSTALLER_GID" "$TP_NOTIFICATION_DATA_DIR"
            run_privileged chmod 0700 "$TP_NOTIFICATION_DATA_DIR"
        fi
    elif ! installer_user_can_read "$TP_NOTIFICATION_DATA_DIR" || ! installer_user_can_write "$TP_NOTIFICATION_DATA_DIR"; then
        error "通知数据目录不可由当前用户读取和写入；请由现有文件所有者或管理员运行升级，不会自动放宽密钥权限"
    fi
    for private_file in infra.json core.env db.env smtp.env migrate.env legacy-reader.env db.pgpass tls/ca.crt tls/server.crt tls/server.key tls/system-ca-bundle.pem; do
        if [ -e "$TP_NOTIFICATION_DATA_DIR/$private_file" ] && ! installer_user_can_read "$TP_NOTIFICATION_DATA_DIR/$private_file"; then
            error "现有通知凭据不可读: $TP_NOTIFICATION_DATA_DIR/$private_file；请由文件所有者或管理员运行，不会自动放宽权限"
        fi
    done
    docker compose run --rm --no-deps notification-setup
    docker compose up -d postgres
    docker compose exec -T postgres sh -c 'until pg_isready -U "$POSTGRES_USER" -d "$POSTGRES_DB"; do sleep 2; done'
    "${INSTALL_DIR}/scripts/notification-db-init.sh"
    "${INSTALL_DIR}/scripts/notification-migrate.sh"

    step "启动或更新服务（保留现有卷和配置）"
    docker compose up -d --wait --timeout 180
    "${INSTALL_DIR}/scripts/notification-nginx.sh"
    success "升级完成！当前版本: $TARGET_VERSION"

    local source_record
    if [ -n "$ASSET_BASE" ]; then source_record="$ASSET_BASE"; else source_record="local:$ASSET_DIR"; fi
    printf 'channel=%s\nref=%s\nsource=%s\n' "$TP_INSTALLER_CHANNEL" "$TP_INSTALLER_REF" "$source_record" > "$marker"
    chmod 0644 "$marker"

    echo ""
    echo -e "${GREEN}✓ ThingsPanel 已成功升级到 ${BOLD}${TARGET_VERSION}${RESET}"
    echo ""
}

main "$@"
