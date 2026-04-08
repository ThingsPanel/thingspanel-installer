# install.bash 设计文档

## 一、安装命令

```bash
curl -fsSL https://install.thingspanel.io/install.sh | bash
```

`install.sh` 是一个极简引导脚本（37行），仅负责下载并执行 `install.bash`。

---

## 二、整体架构

```
┌─────────────────────────────────────────────────────────────┐
│                     用户执行安装命令                          │
│  curl -fsSL https://install.thingspanel.io/install.sh     │
└────────────────────────────┬────────────────────────────────┘
                             ↓
┌─────────────────────────────────────────────────────────────┐
│                   install.sh (引导脚本)                       │
│  · 下载 install.bash 并用 bash 执行                           │
│  · 备用源: GitHub raw                                        │
└────────────────────────────┬────────────────────────────────┘
                             ↓
┌─────────────────────────────────────────────────────────────┐
│                   install.bash (主脚本)                       │
│                                                             │
│  ① 环境检测 → ② 确定版本 → ③ 创建目录 → ④ 下载配置           │
│  → ⑤ 下载脚本 → ⑥ 启动服务 → ⑦ 验证安装                     │
└─────────────────────────────────────────────────────────────┘
```

---

## 三、详细步骤

### 步骤 1：环境检测

| 检测项 | 实现方式 |
|--------|----------|
| 操作系统 | `uname -s` 判断 Linux 或 macOS |
| Docker | 检查 `docker` 命令存在，版本 >= 20.10 |
| Docker Compose | 检查 `docker compose version`，需 v2 |
| 端口占用 | `ss -tuln` 或 `lsof -i` 检查 8080/1883 |
| 内存 | 读取 `/proc/meminfo`（Linux）或 `sysctl`（macOS） |

### 步骤 2：确定版本

```
1. 优先使用环境变量 TP_VERSION
2. 否则从 GitHub API 获取最新版本
   curl https://api.github.com/repos/ThingsPanel/all-in-one-assembler/releases/latest
3. 默认版本: v1.1.13.7
```

### 步骤 3：创建目录

```bash
mkdir -p /opt/thingspanel
```

### 步骤 4：下载 docker-compose.yml

```bash
curl https://install.thingspanel.io/docker-compose.yml \
  -o /opt/thingspanel/docker-compose.yml
```

### 步骤 5：下载管理脚本

下载并添加执行权限：
- `upgrade.sh` — 升级脚本
- `uninstall.sh` — 卸载脚本

### 步骤 6：启动服务

```bash
# 优先级：离线镜像 > 分片镜像 > 在线拉取

# 方式1: 本地完整镜像
if [ -f images.tar ]; then
    docker load -i images.tar
fi

# 方式2: 分片镜像
if [ -f images.tar.part-* ]; then
    cat images.tar.part-* | docker load
fi

# 方式3: 在线拉取（默认）
docker compose pull --quiet
docker compose up -d --wait --timeout 180
```

### 步骤 7：验证安装

```bash
# 等待最多 60 秒，每 2 秒检查一次
curl http://localhost:8080/health
```

---

## 四、可配置项

### 环境变量

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `INSTALL_DIR` | `/opt/thingspanel` | 安装目录 |
| `HTTP_PORT` | `8080` | Web 端口 |
| `MQTT_PORT` | `1883` | MQTT 端口 |
| `TP_VERSION` | `v1.1.13.7` | 安装版本 |

### 使用示例

```bash
# 指定版本和端口
TP_VERSION=v1.2.0 HTTP_PORT=9090 bash install.bash

# 自定义安装目录
INSTALL_DIR=/data/thingspanel bash install.bash
```

---

## 五、离线安装

将镜像包放入安装目录，安装脚本会自动识别：

```
/opt/thingspanel/
├── docker-compose.yml
├── images.tar              # 完整镜像包（优先使用）
├── images.tar.part-001     # 或分片镜像
├── images.tar.part-002
├── images.tar.part-003
├── upgrade.sh
└── uninstall.sh
```

分片镜像支持流式加载，避免合并大文件的磁盘空间问题。

---

## 六、目录结构

安装完成后：

```
/opt/thingspanel/
├── docker-compose.yml   # 主编排文件
├── upgrade.sh           # 升级脚本
├── uninstall.sh         # 卸载脚本
└── data/                 # Docker volume 数据
    ├── postgres/         # 数据库数据
    ├── redis/            # 缓存数据
    ├── gmqtt/           # MQTT 数据
    └── backend/
        ├── files/        # 上传文件
        └── configs/      # 后端配置（需手动初始化）
```

---

## 七、服务列表

| 服务 | 内部端口 | 暴露端口 | 说明 |
|------|----------|----------|------|
| frontend | 8080 | 8080 | 前端界面 |
| postgres | 5432 | 5555 | TimescaleDB |
| gmqtt | 1883 | 1883 | MQTT Broker |
| redis | 6379 | 6379 | 缓存服务 |
| backend | 9999 | 9999 | Go 后端 API |
| modbus_service | 502,503 | 502,503 | Modbus 协议 |
| http_adapter | 19090,19091 | 19090,19091 | HTTP 适配器 |
| thingsvis-server | 8000 | 8000 | 可视化服务 |
| thingsvis-studio | 3000 | 3000 | 可视化编辑器 |

---

## 八、设计原则

| 原则 | 说明 |
|------|------|
| **零配置** | 无需手动编辑文件，下载即运行 |
| **幂等性** | 重复运行只会更新/重启服务 |
| **离线优先** | 检测本地镜像，减少网络依赖 |
| **自动检测** | 自动选择 curl/wget，支持 Linux/macOS |
| **健康检查** | 等待所有容器就绪后再报告成功 |
| **安全默认值** | 数据库密码内置，无需用户设置 |
