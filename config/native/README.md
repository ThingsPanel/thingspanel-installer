# v1.3.0 原生服务配置

本目录配合仓库根 docker-compose.yml；不再使用另一套 Linux 发布包。Encore/通知核心使用 `thingspanel-encore:v1.3.0`，Mosquitto/Owner 使用已有认证插件生产镜像 `thingspanel-mosquitto:v1.3.0`，均由组件仓库发布到阿里云。

部署前在安装目录以管理员执行 `python3 config/native/prepare.py`，生成本机独立凭据，不导入测试服令牌。已有 `.env` 值保留；数据库密码须与已有 PostgreSQL 一致，`TP_NATIVE_DB_PASSWORD` 也须与运行配置中的数据库用户一致。本示例沿用现有 postgres 用户。绑定文件需允许容器 UID 1883 读取。

数据库先沿现有迁移台账准备：社区库应用 v1.3.0 社区/Broker 迁移；另建 device_metadata、automation、notification 三个库，分别应用后端标签 v1.3.0 中对应 migrations。自托管 Encore 镜像不会替你自动执行 SQL。不要在已有库重跑 sql/1.sql。初始化和迁移完成前不要把 compose 配置检查当成服务可用。

`runtime.encore.json` 启动 authn/thingmodel/automation，`runtime.notification.json` 启动 authn/notification；数据库地址为同一 Compose 的 postgres:5432。定时服务每 5 秒调用内部 tick。

现有 MQTT 实现要求平台下发具有明确设备范围；创建/导入设备后执行 `python3 config/native/prepare.py --refresh-devices`，再执行 `docker compose restart broker-owner mosquitto backend` 加载允许列表。空列表仅允许接收上报，不允许任意设备下发；超过 256 个目标需单独规划服务身份。这是当前 Broker 实现的边界，本次不改其鉴权逻辑。协议插件的服务身份也需在 services.json 中显式登记，不能把原 GMQTT 服务登录当成已兼容。

通知源授权、投影、SMTP 插件及持续续期仍沿已有流程配置，不因启动容器自动获得授权。TLS 入口由现有网关配置；此 Compose 的 Mosquitto 只开放 TCP 1883，不再将旧 GMQTT 的 8082/8083 当作 Mosquitto 端口。
