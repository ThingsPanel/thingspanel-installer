#!/usr/bin/env bash
# ─────────────────────────────────────────────────────────────────────────────
# ThingsPanel All-in-One — 升级脚本
# 用法: ./upgrade.sh [版本号]
# 示例: ./upgrade.sh v1.2.12.1
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

check_existing_thingsvis_database() {
    command -v python3 >/dev/null 2>&1 || error "检查已有安装的数据源需要 Python 3；旧配置未修改。"
    python3 - "$INSTALL_DIR/docker-compose.yml" <<'PY' || error "无法读取已有 Compose 配置；旧配置未修改。"
import os, stat, sys
try:
    path = sys.argv[1]
    if not stat.S_ISREG(os.lstat(path).st_mode): raise ValueError('not a regular file')
    with open(path, 'rb') as stream: stream.read(1)
except (OSError, ValueError):
    sys.exit(1)
PY
    if ! (set -o pipefail; cd "$INSTALL_DIR" && { rendered=$(docker compose config --format json 2>/dev/null) || rendered=$(docker compose config --no-interpolate --format json 2>/dev/null); } && printf '%s' "$rendered" | python3 -c '
import json, sys
from urllib.parse import urlsplit

def environment(value):
    if isinstance(value, dict): return value
    if isinstance(value, list):
        result = {}
        for item in value:
            if not isinstance(item, str) or "=" not in item: raise ValueError()
            key, val = item.split("=", 1)
            if key in result: raise ValueError()
            result[key] = val
        return result
    raise ValueError()

try:
    services = json.load(sys.stdin)["services"]
    vis = environment(services["thingsvis-server"]["environment"])
    backend = environment(services["backend"]["environment"])
    url = vis.get("DATABASE_URL")
    if not isinstance(url, str): raise ValueError()
    parsed_url = urlsplit(url)
    if parsed_url.scheme not in ("postgres", "postgresql") or not parsed_url.hostname or "$" in parsed_url.hostname: raise ValueError()
    required = ("GOTP_DB_PSQL_HOST", "GOTP_DB_PSQL_PORT", "GOTP_DB_PSQL_USERNAME", "GOTP_DB_PSQL_DBNAME")
    if any(not isinstance(backend.get(key), (str, int)) or not str(backend[key]).strip() or "$" in str(backend[key]) for key in required): raise ValueError()
    if backend["GOTP_DB_PSQL_HOST"] == "postgres" and not isinstance(services.get("postgres"), dict): raise ValueError()
    selectors = ("GOTP_DB_PSQL_DBTYPE", "GOTP_DB_DRIVER", "GOTP_DB_PROVIDER", "GOTP_DB_TYPE", "DB_DRIVER", "DB_PROVIDER", "DB_TYPE", "DATABASE_TYPE", "DATABASE_PROVIDER", "DATABASE_DRIVER")
    for key, value in backend.items():
        name = str(key).upper()
        if name.startswith(("GOTP_DB_MYSQL_", "GOTP_DB_MARIADB_")) and value not in (None, ""): raise ValueError()
        if name in selectors and str(value).lower() not in ("postgres", "postgresql", "pg"): raise ValueError()
        if name == "DATABASE_URL" and (not isinstance(value, str) or urlsplit(value).scheme not in ("postgres", "postgresql")): raise ValueError()
    sys.exit(0)
except Exception:
    sys.exit(1)
'); then
        error "无法确认已有平台和 ThingsVis 使用受支持的 PostgreSQL 配置；旧配置未修改。"
    fi
}

main() {
    echo -e "${BOLD}ThingsPanel — 升级程序${RESET}"
    echo ""

    check_existing_thingsvis_database

    TARGET_VERSION="${1:-}"
    if [ -z "$TARGET_VERSION" ]; then
        TARGET_VERSION=$(curl -fsSL \
            "https://api.github.com/repos/${REPO}/releases/latest" \
            2>/dev/null | grep '"tag_name"' | head -1 | sed 's/.*"tag_name": *"\([^"]*\)".*/\1/' || true)
    fi
    if [ -z "$TARGET_VERSION" ]; then
        error "无法获取最新版本，请手动指定：./upgrade.sh v1.2.12.1"
    fi
    info "目标版本: $TARGET_VERSION"

    step "备份配置文件"
    if [ -f "${INSTALL_DIR}/docker-compose.yml" ]; then
        cp "${INSTALL_DIR}/docker-compose.yml" "${INSTALL_DIR}/docker-compose.yml.bak.$(date +%Y%m%d%H%M%S)"
    fi
    success "已备份"

    step "更新配置文件"
    curl -fsSL "${RAW_BASE}/docker-compose.yml" -o "${INSTALL_DIR}/docker-compose.yml"
    mkdir -p "${INSTALL_DIR}/config/native"
    curl -fsSL "${RAW_BASE}/niginx.conf" -o "${INSTALL_DIR}/niginx.conf"
    for native_file in prepare.py runtime.encore.json runtime.notification.json tick.sh README.md; do
        curl -fsSL "${RAW_BASE}/config/native/${native_file}" -o "${INSTALL_DIR}/config/native/${native_file}"
    done
    command -v python3 >/dev/null 2>&1 || error "v1.3.0 原生配置准备需要 Python 3"
    python3 "${INSTALL_DIR}/config/native/prepare.py"
    success "配置文件已更新"

    step "拉取新镜像"
    cd "$INSTALL_DIR"
    python3 "${INSTALL_DIR}/config/native/prepare.py" --pull
    success "镜像拉取完成"

    step "准备数据库和服务配置"
    python3 "${INSTALL_DIR}/config/native/prepare.py" --bootstrap --refresh-devices

    step "重启服务"
    docker compose up -d --wait --wait-timeout 240
    python3 "${INSTALL_DIR}/config/native/prepare.py" --verify
    success "升级完成！当前版本: $TARGET_VERSION"

    echo ""
    echo -e "${GREEN}✓ ThingsPanel 已成功升级到 ${BOLD}${TARGET_VERSION}${RESET}"
    echo ""
}

main "$@"
