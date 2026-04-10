# ThingsPanel All-in-One

> 一行命令，部署完整 ThingsPanel IoT 平台

[![Release](https://img.shields.io/github/v/release/ThingsPanel/all-in-one-assembler)](https://github.com/ThingsPanel/all-in-one-assembler/releases)
[![Validate](https://github.com/ThingsPanel/all-in-one-assembler/actions/workflows/validate.yml/badge.svg)](https://github.com/ThingsPanel/all-in-one-assembler/actions)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue)](LICENSE)

---

## 快速安装

### Linux / macOS

```bash
curl -fsSL https://install.thingspanel.io/install.sh | sh
```

安装完成后访问：**http://localhost:8080**

---

### Windows

1. 下载最新 [ThingsPanel-Setup.exe](https://github.com/ThingsPanel/all-in-one-assembler/releases/latest)
2. 右键 → **以管理员身份运行**
3. 按向导完成安装

> 前置要求：[Docker Desktop for Windows](https://www.docker.com/products/docker-desktop)

---

### macOS（图形界面）

1. 下载最新 [ThingsPanel-x.x.x.pkg](https://github.com/ThingsPanel/all-in-one-assembler/releases/latest)
2. 双击安装
3. 等待安装完成，访问 **http://localhost:8080**

> 前置要求：[Docker Desktop for Mac](https://www.docker.com/products/docker-desktop)

---

## 包含的服务

| 服务 | 说明 | 端口 |
|------|------|------|
| **frontend** | ThingsPanel Vue 前端（镜像内置 nginx） | 内部 |
| **backend** | ThingsPanel Go 后端 API | 内部 |
| **postgres** | TimescaleDB 时序数据库 | 内部 |
| **redis** | 缓存 | 内部 |
| **gmqtt** | MQTT Broker | 内部 |
| **modbus** | Modbus 协议插件 | 502/503 |
| **http-adapter** | HTTP 协议适配器 | 内部 |
| **thingsvis-server** | ThingsVis 大屏服务端 | 内部 |
| **thingsvis-studio** | ThingsVis 大屏编辑器 | 内部 |
| **gateway** | Nginx 统一入口（镜像内置 nginx） | **8080** |

---

## 系统要求

| 项目 | 最低 | 推荐 |
|------|------|------|
| 内存 | 2 GB | 4 GB |
| CPU | 2 核 | 4 核 |
| 磁盘 | 10 GB | 20 GB |
| Docker | 20.10+ | 最新版 |
| Docker Compose | v2.0+ | 最新版 |

---

## 常用命令

```bash
# 查看所有服务状态（健康/不健康 一目了然）
docker compose -f /opt/thingspanel/docker-compose.yml ps

# 查看后端日志
docker compose -f /opt/thingspanel/docker-compose.yml logs -f backend

# 查看所有日志
docker compose -f /opt/thingspanel/docker-compose.yml logs --tail=100

# 重启某个服务
docker compose -f /opt/thingspanel/docker-compose.yml restart backend

# 停止所有服务
docker compose -f /opt/thingspanel/docker-compose.yml down

# 启动所有服务
docker compose -f /opt/thingspanel/docker-compose.yml up -d

# 删除所有服务、镜像、数据、网络
docker compose -f /opt/thingspanel/docker-compose.yml down --rmi all -v --remove-orphans

```

---

## 升级

```bash
# Linux / macOS
/opt/thingspanel/upgrade.sh

# 指定版本
/opt/thingspanel/upgrade.sh v1.2.0

# Windows (管理员 PowerShell)
C:\ThingsPanel\upgrade.ps1
```

---

## 卸载

```bash
# Linux / macOS（保留数据）
/opt/thingspanel/uninstall.sh

# 彻底删除（含数据库）
/opt/thingspanel/uninstall.sh --purge

# Windows (管理员 PowerShell)
C:\ThingsPanel\uninstall.ps1 -Purge
```

---

## 配置

安装后配置文件位于 `/opt/thingspanel/.env`（Windows：`C:\ThingsPanel\.env`）。

修改配置后执行以下命令使其生效：

```bash
docker compose -f /opt/thingspanel/docker-compose.yml up -d
```

完整配置说明请参考 [docs/configuration.md](docs/configuration.md)。

---

## 文档

- [配置说明](docs/configuration.md)
- [升级指南](docs/upgrade.md)
- [故障排查](docs/troubleshooting.md)

---

## 架构说明

```
用户请求 :8080
     │
     ▼
┌────────────────────────────────────┐
│  gateway (前端镜像内置 nginx)        │
│  /         → 静态文件 SPA          │
│  /api/     → backend:9999         │
│  /mqtt     → gmqtt:1883 (WS)      │
│  /thingsvis/ → thingsvis-server    │
│  /main/    → thingsvis-studio     │
└────────────────────────────────────┘
     │
     ▼
 frontend（前端镜像，同一个 nginx）
     │
     ▼
 backend        thingsvis
 (Go API)      (Server)
```

---

## 贡献

欢迎提 Issue 和 PR！本仓库专注于 All-in-One 打包和分发，功能性问题请到对应子项目提 Issue。

- 后端：[thingspanel-backend-community](https://github.com/ThingsPanel/thingspanel-backend-community)
- 前端：[thingspanel-frontend-community](https://github.com/ThingsPanel/thingspanel-frontend-community)

---

## License

[Apache 2.0](LICENSE)
