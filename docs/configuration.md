# configuration.md
# ThingsPanel 配置说明

所有配置通过安装目录下的 `.env` 文件管理。

## 变量列表

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `TP_VERSION` | 自动 | ThingsPanel 核心版本号 |
| `POSTGRES_PASSWORD` | 随机生成 | 数据库密码（勿手动修改已部署环境）|
| `REDIS_PASSWORD` | 随机生成 | Redis 密码 |
| `AUTH_SECRET` | 随机生成 | ThingsVis 认证密钥（≥32位）|
| `DATA_DIR` | `./data` | 数据持久化目录 |
| `HTTP_PORT` | `8080` | Web 访问端口 |
| `MQTT_PORT` | `1883` | MQTT 设备接入端口 |
| `MODBUS_TCP_PORT` | `502` | Modbus TCP 端口 |
| `MODBUS_RTU_PORT` | `503` | Modbus RTU 端口 |
| `TZ` | `Asia/Shanghai` | 时区 |
| `TP_LOG_LEVEL` | `error` | 日志级别（debug/info/warn/error）|

## 修改配置后重启

```bash
docker compose -f /opt/thingspanel/docker-compose.yml up -d
```

## 数据目录结构

```
data/
├── postgres/          # 数据库文件（勿手动修改）
├── redis/             # Redis 持久化文件
├── gmqtt/             # MQTT Broker 配置和数据
└── backend/
    ├── files/         # 上传的文件
    └── configs/       # 后端自定义配置
```
