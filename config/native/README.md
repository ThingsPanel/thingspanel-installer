# v1.3.0 一键安装

Linux 服务器准备好 Docker Compose v2、Python 3 和 OpenSSL 后，执行原来的命令：

```sh
curl -fsSL https://install.thingspanel.io/install.sh | sh
```

完成后访问 `http://服务器地址:8080`。安装目录默认 `/opt/thingspanel`，不需要手工生成凭据、建库、执行 SQL 或分批启动服务。

脚本自动下载本仓库 Compose 和配置，逐个拉取镜像，为本机生成持久凭据及内部 TLS 证书，启动 PostgreSQL/Redis，执行镜像携带的尚未执行迁移，再启动服务并检查实际 API、通知认证和定时唤醒。失败会退出，不会重新运行整套安装器或改用其他镜像名。

社区数据库按 `sys_version` 升级；Broker 迁移及 `device_metadata`、`automation`、`notification` 三个资源库按迁移记录执行。自托管 Encore 镜像不自动执行 SQL，由安装脚本完成。重复安装保留 `.env`、数据和迁移记录；已有库密码必须与 `.env` 一致。凭据只保存在本机，不提交仓库。

Encore/通知核心共用 `thingspanel-encore:v1.3.0`；Mosquitto/Owner 共用 `thingspanel-mosquitto:v1.3.0`，分别运行不同进程。后端镜像也被定时唤醒进程复用。所有镜像由组件仓库的发布 Action 上传阿里云，安装器负责拉取与启动。

通知未配置插件时可管理通知页面，外发全部拒绝；实际发送仍需配置通知渠道和 SMTP。内部 TLS 的 8443 端口不对宿主机开放。MQTT 默认是 TCP 1883，不提供旧 GMQTT 的 WebSocket 端口。

HTTP/Modbus 使用安装时生成的独立协议服务账号。新装没有设备时只允许建立连接，不获得任何发布或订阅权限。平台命令和协议插件的数据访问仍限于明确登记的设备；创建或导入设备后，维护人员在安装目录执行：

```sh
python3 config/native/prepare.py --refresh-devices
docker compose restart broker-owner mosquitto backend http_adapter modbus_service
```

单个服务允许列表最多 256 个设备；超出需规划独立服务身份。以上是当前 Broker 的设备范围管理要求，不是安装步骤。

查看运行状态和错误：

```sh
cd /opt/thingspanel
docker compose ps
docker compose logs --tail=100
```
