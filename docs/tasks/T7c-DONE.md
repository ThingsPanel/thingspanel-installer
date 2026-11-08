# T7c — 修复 T7 发现的三处安装体验缺口

未推送、未合并。分支：安装器 `wp/t5-identity`；HA 插件 `wp/t6-ha-package`（`5394d9c`）。SDK、后端未改。

## 做了什么
1. **安装器自动登记服务插件**（`tools/plugin_install.py`）：目录条目的 `platform.service_plugin`（`name`、`service_type`、`config`）声明要登记的字段，`config` 里可用 `{alias}`、`{http_port}`。`binding: none` 安装时用租户登录经平台现有 API 幂等登记：先分页 `GET /api/v1/service/list` 按 `service_identifier` 精确查找；没有则 `POST /api/v1/service`，有则 `PUT` 刷新声明的字段（保留已有的其他配置键和备注）。HA 条目声明 `sub_topic_prefix=plugin/homeassistant/`、`http_address={alias}:{http_port}`。目录校验：`service_plugin` 只允许 `binding: none`，`http_address` 要求有 `http_port`。
   - **卸载保留这条记录，只吊销身份**（Agent 吊销 + `identity/revoke`）。理由：设备、接入点通过 `service_plugin_id` 引用它，删除会让它们变孤儿；重装可复用同一条记录。已验证卸载后记录仍在。
2. **稳定地址**：Agent profile 新增 `network_alias`（`agent/prepare.go`、`agent/docker_prepare.go`），创建容器时通过 Docker `NetworkingConfig` 加网络别名，必须同时有 `network`，格式限制为小写 DNS 标签；巡检（`safePreparedContainer`）会校验别名存在，缺失视为不可信。安装器给每个插件用 `tp-plugin-<plugin-id>`（可由 `platform.network_alias` 覆盖），安装结束的检查增加“经别名访问健康接口”。平台登记的 `http_address` 只用别名（`tp-plugin-homeassistant:9001`）。
3. **HA 插件周期同步**（`startup_sync.go`）：启动同步后，每 `HA_DEVICE_SYNC_INTERVAL_SECONDS`（默认 60，下限 5，`0` = 只在启动时同步）通过 SDK `ServiceAccessList` 拉取，只对新增设备调 `OnDeviceAdd`、对配置变化的设备调 `OnConfigUpdate`（设备配置来自 access list 里的 `voucher/protocol_config`，与原启动同步一致）。`sdk.IsAuthFailure`（无身份/503 身份/403 越权）→ 记一条错误并永久停止，不重试；其他错误按正常间隔继续，启动阶段沿用“最多重试 5 次”。同步状态（次数、设备数、设备号，无凭据）在 `GET :9002/sync-status`（`HA_STATUS_ADDR`，`off` 关闭），因为插件容器日志驱动是 none，看不到日志。

## 测试
- 安装器 Python：`python3 -m unittest tools/test_plugin_install.py` → 18 项 OK（新增 6 项：别名与配置填充、重复安装只有一行、修复旧地址并保留他人键、不碰其他 identifier、模板型不登记、目录校验）。
- Agent Go：新增 `agent/network_alias_test.go`（别名校验、跨代次别名一致、缺别名的容器不被信任）；整个 agent 包 `GOOS=linux` 编译成测试二进制放进容器里跑（包是 linux 构建标签）→ PASS。
- HA 插件：`go vet ./... && go test ./...` → ok；新增 `startup_sync_test.go`：新增/变更/不变、空列表、三种身份错误只调用 1 次且状态 stopped、瞬时错误后继续拉取、间隔解析。
- **最小栈回归**（`tools/regress_t7c.py`，栈 `/Volumes/500G/t7c-stack`：develop 563e2497 后端 + timescaledb + redis + 匿名 mosquitto，无 Broker 数据链路，HA 地址/令牌是假的）：`regress-3.log` 全 PASS：
  0 安装前无记录 → 1 一条命令安装 → 2 自动出现 1 条记录且 config 含 `sub_topic_prefix`、`http_address=tp-plugin-homeassistant:9001` → 3 平台用登记地址调插件（`voucher/form`）HTTP 200 → 4 同步在跑 → 5 平台新建设备 **41 秒**后插件感知（`/sync-status` devices=1）→ 6 重装（代次升高）→ 7 记录仍 1 条、id 不变 → 8 换代后地址仍可用 → 9 只有 1 个插件容器 → 10 重装后新建设备 **45 秒**内被感知。
  另手工验证卸载后记录仍在（`row-after-uninstall.log`）。
- 证据：`/Volumes/500G/archive/evidence/2026-11-04-T7c/`（日志无令牌，grep 为 0）。

## 没做 / 注意
- 回归没走真实 Broker 和真实 HA（上行/下行属 T7b 之后的事）；“感知”以插件 `/sync-status` 为准，不是遥测。
- 同步不处理设备删除（只增、改）；平台删除设备后插件仍保留内存记录。
- 周期同步是整表拉取，设备很多时每分钟一次，量大时再考虑增量。
- 安装器的 Agent 二进制必须用本分支重新构建（旧二进制会忽略 `network_alias`，别名不生效，回归中踩过一次）；Agent 容器名 `tp-agent-<project>` 和插件容器 `tp-agent-prepared-*` 是代码固定的，没有 `t7c-` 前缀，已全部删除。
- 回归中途有外部进程对 `tp-agent-prepared-*` 容器执行过 `docker restart`（不是本任务，事件里只有 restart），把已停止的旧代次容器拉起过一次；我已清掉。若这是另一会话的批量操作，请留意它会复活被 Agent 停掉的旧代次容器。
- `plugins/homeassistant.json` 现指向 `thingspanel-homeassistant:t7c-arm64`（`sha256:43984ca4…`，本机用 `Dockerfile.local` 构建，镜像已随栈清理删除，需要时重建；依赖本地 SDK replace，同 T5）。HA token 仍明文进后端 `service_access.voucher`（T7 已记录，未处理）。
- 平台服务插件列表接口必须带 `page`/`page_size`，否则报 `Field 'Page' is required`；安装器已分页。
- 并发安装同一插件有极小概率建出两行（先查后建，无唯一约束）；单机一条命令的场景不会出现。
