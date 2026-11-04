# T7 — HA 一键安装端到端：数据打通

**结论：第 1、2、5、6 步通过；第 3、4 步（上行、下行）没有通过，没有任何数据进入平台。** 根因在后端/Broker 发行物，不在安装器和 HA 插件，按任务规则停下记录，没有扩大范围。未推送、未合并；本任务没有修改任何仓库的代码（只新增这份说明）。

证据目录（不进仓库）：`/Volumes/500G/archive/evidence/2026-11-04-T7/`（80G 盘写入慢，按总监要求放 500G；需要负责人挪动）。栈目录 `/Volumes/500G/t7-stack/`（只剩 compose 文件和二进制，容器、卷、网络、凭据已删）。

## 环境
- 后端：develop `563e2497` 交叉编译 linux/arm64，挂进 v1.3.0 后端镜像；timescaledb pg16、redis、Home Assistant（`ghcr.io/home-assistant/home-assistant:stable`，demo 集成 + 虚拟实体）。
- **`sql/33.sql` 是手工 `psql` 执行的**（后端启动不会执行，安装流程也没有这一环）；`sql/broker/001–017` 同样手工执行。
- Broker：先用 `thingspanel-mosquitto:v1.3.0` + broker-owner，带 `TP_PROTOCOL_MQTT_USER/PASSWORD`（见第 3 步）。
- 安装器 `wp/t5-identity`（`e0102f9`），插件镜像 `thingspanel-homeassistant:t5-arm64`（`sha256:f7a78a7e…`），T5 的测试密钥目录。
- HA 的 long-lived token 只经环境变量传给安装命令；测试结束已在 HA 里撤销（旧 token 访问返回 401，`ha-token-revoked.log`）。

## 逐步结果
| 步 | 结果 | 证据 |
| --- | --- | --- |
| 1 一条命令安装（只给 HA 地址和 token） | **通过**。generation 1，容器 `/health` 返回 ok；后端日志里 `plugin/heartbeat`、`plugin/service/access/list` 均 200，MQTT 命令桥连上 Broker | `step1-install.log` |
| 2 发现 HA 实体并在平台生成设备 | **通过，但不是自动的**：插件列出 33 个实体，我用平台 API 注册服务插件、建接入点、批量建了 3 台设备（灯 Bed Light、温度、湿度）。安装本身不会建任何平台对象 | `step2-discovered.json`、`step2-create-devices.log` |
| 3 上行（HA 改值 → 平台当前值/历史） | **失败**，平台 0 条遥测 | `step3-real-broker-denied.log` |
| 4 下行（平台开关灯 → HA 变化 → 回执） | **未能测试**：设备/插件在真实 Broker 上连不上，命令发不出去 | 同上 |
| 5 重复安装 | **通过**：generation 升到 2，旧容器被替换，始终只有 1 个插件容器；设备仍是 3 台，接入点 1 个、服务插件行 1 个；旧代次身份已吊销 | `step5-reinstall.log` |
| 6 卸载 | **通过（历史数据部分无法验证）**：身份 1、2 均已吊销，容器 Exited，3 台设备和接入点保留，用旧令牌调心跳返回 503。因为第 3 步没有数据，“历史数据保留”没有被验证 | `step6-uninstall.log` |

## 第 3、4 步失败的根因（后端 / Broker，不在 1 小时内可修）
1. **发行版 Broker 镜像比 develop 旧，插件设备连不上。** HA 插件的上行是“每台设备用自己的凭据（voucher 里的 username）连 Broker 发 `devices/telemetry`”，命令桥用服务账号订阅 `plugin/homeassistant/devices/command/+/+`。在 `thingspanel-mosquitto:v1.3.0`（构建于 10-29）上：
   - 服务账号带设备范围、设备自带凭据，都返回 CONNACK 5（not authorised）；去掉设备范围后服务账号能连上，说明是“设备范围解析”被拒。
   - 把 broker-owner 换成用 `563e2497` 构建的 `tp-broker-owner`（含 `7bfaff53`）后，owner 开始正常发放授权（`broker_plugin_contexts`、`broker_transport_bindings` 有新行），但 Mosquitto 仍返回 CONNACK 5。推断是镜像里的 `tp-mosquitto-plugin.so` 也需要用 develop 重建（`deploy/mosquitto-community/Dockerfile.production`）。这是推断，没有重建验证。
2. **换成不带身份映射的普通 Mosquitto 也不行**：插件发布的 `devices/telemetry` 载荷里没有 `device_id`，后端报 `device_id cannot be empty`。也就是说 HA 插件依赖 Broker 按凭据注入设备身份，不能脱离新 Broker 使用。
3. 修复需要：用 develop 重新构建并发布 `thingspanel-mosquitto` 镜像（owner + 插件库），安装器的 `config/native/prepare.py` 里服务账号的设备范围（`--refresh-devices`）也要在“建完 HA 设备”之后刷新并重启 broker-owner/mosquitto。这部分属于后端/发行物，交总监决定。

## 其他发现（没有修，需要排期）
- **安装不会在平台登记服务插件**：`binding: none` 时平台没有 `service_plugins` 行，必须有人手工建，且 `service_config` 必须带 `sub_topic_prefix: plugin/homeassistant/`（Broker 和下行路由都用它）和 `http_address`。建议安装器在 `binding: none` 的服务型插件上幂等登记。
- **`http_address` 在重装后失效**：Agent 容器名带代次哈希（`tp-agent-prepared-<hash>`），第 2 代起平台里登记的地址指向已删除的容器。需要稳定的网络别名或登记时用别名。
- **插件只在启动时同步设备**（`startup_sync` 只跑一次）：之后在平台新建的设备插件不知道。我为让它看到设备手工 `docker restart` 了一次插件容器。建议插件周期性拉 `service/access/list`。
- **后端会把 HA token 写进日志和操作日志表**：创建接入点时 voucher 整段进入请求日志，设备列表请求出错时 voucher 被拼进 URL 并打到错误日志。另外 voucher 里的 token 会明文存进 `service_access.voucher`。安装时只给一次 token、平台里又要重输一次，也是体验问题。
- 后端用字符串拼接把 voucher 放进 URL，不做编码：voucher 必须是紧凑 JSON（无空格），否则插件返回 400。
- 平台服务接入点列表接口（`service/access/list`）对该租户返回空列表，但创建后可以直接用 ID 操作；没有深究。
- 第 7 步（TDengine，可选）没做。

## 修了什么
没有修改任何仓库（安装器、SDK、HA 插件、后端都没有新提交）。

## 复现/清理
- 栈：`/Volumes/500G/t7-stack/docker-compose.yml` + `broker.yml`（容器名均带 `t7-`；Agent 创建的插件容器名是 `tp-agent-*`，已一并删除）。
- 收尾已执行 `compose down -v`、`docker rm -fv`，网络和 `t7-stack_mosquitto_data` 卷已删；`tp-develop-local` 未触碰。
