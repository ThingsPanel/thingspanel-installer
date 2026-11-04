# T6 — HA 插件打包进签名目录

分支：installer `wp/t6-ha-catalog`（基于 `wp/i1-pg-profiles` @ 6e5f356）；HA 插件仓库 `wp/t6-ha-package`（基于 4d8d45b，提交 aff6a16）。未推送。

## 做了什么
- **镜像**（Dockerfile 无需修改，仅加 `.dockerignore`）：
  - linux/arm64 `sha256:ee93832cc57d93424f050f359e9bc716ff2dddfaff6075ddbb4b20d3132fcbba`（目录条目固定此值）
  - linux/amd64 `sha256:fff8b89185a131b2530f1366682cec0940de005ce3760b5c15f28316bb2c6ba4`（已构建，未入目录；多架构需 registry 摘要）
  - 镜像仅在本机，未推送 registry。
- **目录条目** `plugins/homeassistant.json`：端口 9001、健康检查 `/health`；用户必填 `env_user`：`HA_BASE_URL`、`HA_ACCESS_TOKEN`（secret）；自动填写 `CONNECTOR_SERVICE_IDENTIFIER=homeassistant`、`CONNECTOR_INSTANCE_ID=homeassistant-1`、`THINGSPANEL_BACKEND_URL=http://backend:9999`、`TP_MQTT_BROKER=tcp://mosquitto:1883`，MQTT 账号来自栈 `.env` 的 `TP_PROTOCOL_MQTT_USER/PASSWORD`。不指向 gmqtt。
- `platform` 块写成 `{"binding":"none",...}` 仅为通过 `check_entry`（它要求该键存在）。

## 验证
- `plugin_catalog.py publish`（本地测试密钥，未提交）→ `verify`：`catalog vt6-1 verified: homeassistant@1.0.0-t6, http-adapter@1.2.0-identity`。篡改版本号后验签：`bad_signature`。
- Agent 安装：在隔离 docker 网络 `t6-ha-net` 上，用 `plugin_install.Agent` 走 ensure_running → accept-generation → prepare → grant → activate，generation 1 运行；对容器 `GET :9001/health` 返回 `{"instanceID":"homeassistant-1","serviceIdentifier":"homeassistant","status":"ok"}`。测试容器与网络已删除。

## 没做到 / 偏差
- **没有用 `plugin_install.py install` 一条命令**：它固定创建协议设备模板，并在 `agent_install` 里向 `/api/v1/plugin/runtime/identity` 申请身份（6e5f356 WIP，T4/T5 范围，我没改）。HA 没有协议模板，所以我用同一 Agent 调用序列的草稿脚本（不入库）替代。让一条命令可用需要 install 支持 `binding: none`、`env_user` 提示/校验，建议在 T5 一并做。
- **没有对接真实平台栈**：本机 `tp-develop-local` 是别人的集成夹具，无 `TP_PROTOCOL_MQTT_*`，网络名不同，不去碰。因此 MQTT 用假账号、HA 地址/令牌是假值；MQTT 连接、取设备配置（预期 503）、与 HA 真实通信均未验证。
- 只验证了 arm64；amd64 镜像未运行。
- HA 仓库 main 上有未提交改动（handler.go、ha_client.go、main.go、startup_sync.go、README.md、ha_actions.go、capabilities_test.go），镜像基于已提交的 4d8d45b，不含这些。
- 未注册 worktree 到 `docs/WORKSPACES.md`（该文件在后端仓库，本任务不改后端）；需负责人登记：两个 worktree 路径见上。
