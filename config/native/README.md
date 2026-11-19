# 社区版原生服务安装

Linux 服务器准备好 Docker Compose v2、Python 3 和 OpenSSL 后，执行原来的命令：

```sh
curl -fsSL https://install.thingspanel.io/install.sh | sh
```

完成后访问 `http://服务器地址:8080`。安装目录默认 `/opt/thingspanel`，不需要手工生成凭据、建库、执行 SQL 或分批启动服务。

脚本自动下载本仓库 Compose 和配置，逐个拉取镜像，为本机生成持久凭据及内部 TLS 证书，启动 PostgreSQL/Redis，执行镜像携带的尚未执行迁移，再启动服务并检查实际 API、通知认证和定时唤醒。失败会退出，不会重新运行整套安装器或改用其他镜像名。

社区数据库按 `sys_version` 升级；Broker 迁移及 `device_metadata`、`automation`、`notification` 三个资源库按迁移记录执行。自托管 Encore 镜像不自动执行 SQL，由安装脚本完成。重复安装保留 `.env`、数据和迁移记录；已有库密码必须与 `.env` 一致。凭据只保存在本机，不提交仓库。

Encore/通知核心共用 `thingspanel-encore:v1.4.0`；Mosquitto/Owner 共用 `thingspanel-mosquitto:v1.4.0`，分别运行不同进程。后端镜像也被定时唤醒进程复用。所有镜像由组件仓库的发布 Action 上传阿里云，安装器负责拉取与启动。

通知未配置插件时可管理通知页面，外发全部拒绝；实际发送仍需配置通知渠道和 SMTP。内部 TLS 的 8443 端口不对宿主机开放。MQTT 默认是 TCP 1883，不提供旧 GMQTT 的 WebSocket 端口。

受管插件目录使用官方签名。安装器按 Docker 架构验证目录、Agent 和目录中各镜像的摘要，导入镜像后检查实际镜像 ID 和架构。已激活目录在新版下载失败时可继续使用。Linux 上 Agent 启用后，Broker 范围同步会跟踪设备以及受管插件的有效安装代次。受管 HTTP 和 Modbus 使用各自的 MQTT 账号，空设备范围只允许连接。

当前 Compose 为全新安装准备受管插件入口。超管在「设备 → 插件安装」启用插件后，HTTP 设备可访问 19090 端口的上报、轮询和回执接口，Modbus 网关从 502 端口连接。插件健康和表单回调在 Compose 网络内访问。

公开站点当前发布 Home Assistant 目录。发布此 Compose 时需要同时交付 amd64、arm64 的三插件签名目录和对应 Release 制品，才能从页面安装 HTTP 和 Modbus。受管 Modbus 制品流程见 [Modbus 接入说明](../../docs/MODBUS-MANAGED.md)。

查看运行状态和错误：

```sh
cd /opt/thingspanel
docker compose ps
docker compose logs --tail=100
```
