# T5 — 安装器与 SDK：签发身份并带上身份

未推送、未合并。接口依据：T4 的 `docs/plugin-runtime-identity.md`。

## 三个仓库
| 仓库 | 分支 / 提交 |
| --- | --- |
| 安装器 `/Users/junhong/Downloads/code/thingspanel-installer`（worktree `thingspanel-installer-wp-t5`） | `wp/t5-identity`，基于 `wp/t6-ha-catalog`：`b9639de`（身份/binding none/env_user/uninstall）、`71647da`（HA 条目指向 SDK v0.2.0 镜像）、`d60464b`（卸载后重装接管、回归脚本） |
| SDK（新目录 `/Users/junhong/Downloads/code/thingspanel-device-connector-sdk-go`） | 来源：`go mod download` 取得的 v0.1.0 源码（原仓库 commit 0ee1d177），`git init`，`e3692bb` 为原样导入并 tag `v0.1.0`；`6c744a2` 为本任务改动，tag **`v0.2.0`**（本地，未推送） |
| HA 插件（worktree `.claude/worktrees/t6`） | `wp/t6-ha-package`：`cb6c4de`（SDK→v0.2.0） |

## 做了什么
- **签发/吊销（安装器）**：`agent_install` 每次安装/升级用 `generation = 旧值+1` 向 `POST /api/v1/plugin/runtime/identity` 签发；`uninstall` 先让 Agent revoke 再调 `identity/revoke`；代次计数保留在 Agent 插件状态文件里，所以重装一定比任何已吊销代次高。Agent 步骤失败时会尽力吊销刚签发的身份。状态文件丢失导致 409 时给出明确提示。
- **注入**：身份只通过 Agent profile 的容器环境变量注入：`TP_PLUGIN_RUNTIME_TOKEN`（目录条目可用 `identity.token_env` 改名，http-adapter 用 `P_PLATFORM_API_TOKEN`）和 `TP_PLUGIN_SERVICE_IDENTIFIER`。不进镜像、不打印；`profiles.json` 在容器创建后立即重写为不含令牌（实测 `grep pri_` = 0）。令牌仍在容器配置里（`docker inspect` 可见），这是环境变量方式的固有性质。
- **`binding: none`**：不创建协议模板，不支持 `--device`（明确报错）；有 `health` 时安装结束会探测健康检查。`plugin_catalog.check_entry` 校验 binding 与必填字段。
- **`env_user`**：`--set KEY=VALUE`（非 secret）→ 同名环境变量 → 条目 `default` → 终端交互提示（secret 用 getpass 不回显）；`--non-interactive` 缺项即失败。secret 字段拒绝 `--set`（避免进 ps / shell 历史）。支持 `validate: url`、`pattern`；拒绝换行/NUL。用户值不落盘。
- **SDK v0.2.0**：新增 `PlatformClient`（`Heartbeat/DeviceConfig/Devices/ServiceAccessList/ServiceAccess/Post`），统一带 `Authorization: Bearer`；无令牌时不发请求（`ErrNoRuntimeIdentity`）；503 `PLUGIN_RUNTIME_IDENTITY_REQUIRED`→`ErrIdentityRequired`，403→`ErrScopeDenied`，`IsAuthFailure` 为真即不重试；内置心跳循环收到后只记一条错误并停止。`ConnectorInfo` 带 `RuntimeToken`，`%v/%+v/%#v` 打印脱敏。`FromEnv` 读 `TP_PLUGIN_RUNTIME_TOKEN` / `_FILE`、`TP_PLUGIN_SERVICE_IDENTIFIER`（优先于 `CONNECTOR_SERVICE_IDENTIFIER`）。
- **HA 插件**：`go.mod` 升到 v0.2.0；`startup_sync` 改用 SDK 的 `Platform()` 客户端，遇身份/越权错误立即停止重试。

## 测试与结果
- SDK：`go vet ./... && go test -count=1 ./...` → ok（含新增 7 个用例：5 入口带 Bearer、无令牌不发请求、503/403 区分、普通 503 可重试、心跳在被拒后只调用 1 次、令牌不进格式化输出、令牌文件）。
- 安装器：`python3 -m unittest tools/test_plugin_install.py` → 12 项 OK。
- HA：`go vet ./... && go test ./...` → ok（SDK 走本地 replace）。
- **回归（真实栈）**：`tools/regress_identity.py`，http-adapter，栈 `t5-stack`（T4 worktree 构建的后端二进制 + timescaledb pg16 + redis + 匿名 mosquitto，已 `compose down -v` 删除；未碰 `tp-develop-local`）。最终日志 `regress-3.log`，全部 PASS：
  1. 安装 → 代次 1 成功；2. 容器环境里有 `pri_<id>.<secret>`；3. 带新身份取设备配置 HTTP 200（无身份 503 `PLUGIN_RUNTIME_IDENTITY_REQUIRED`）；4. 卸载成功；5. 旧身份 → 503，容器已停；6. 重装 → 代次 2；7. 旧身份 503、新身份 200。
- **HA 一条命令**（`ha-install.log`）：`plugin_install.py install --plugin homeassistant --set HA_BASE_URL=... `（`HA_ACCESS_TOKEN` 走环境变量）→ binding none，容器 `/health` 返回 ok；后端日志显示 HA 的 `plugin/heartbeat`、`plugin/service/access/list` 均 200，库中身份 `homeassistant|homeassistant|1`；随后卸载，身份被吊销。
- 证据放在 `/Volumes/500G/t5-stack/evidence/`（`regress-1/2/3.log`、`ha-install.log`、`lost-state-409.log`）。`/Volumes/80G` 在本次中途写入卡死（dd 超时），所以没有放进约定的 `/Volumes/80G/archive/evidence/2026-11-04-T5/`（该目录里只有 build/test 日志）；需要负责人挪动。

## 与 HA 镜像
- 新镜像（arm64，本机）`sha256:f7a78a7e32ceb6b5f72f3f3de634e32040e695346c519713125d744324e93d08`，tag `thingspanel-homeassistant:t5-arm64`，目录条目 `plugins/homeassistant.json` 已更新（版本 `1.0.0-t5`）。
- **构建依赖本地 SDK**：`go.mod` 里有 `replace … => ../../../../thingspanel-device-connector-sdk-go`（仅本地开发；SDK 推送并发布 v0.2.0 后必须删除）。镜像用 `Dockerfile.local` 构建：
  `docker build --platform linux/arm64 --build-context sdk=<SDK 目录> -f Dockerfile.local -t thingspanel-homeassistant:t5-arm64 .`
  原 `Dockerfile` 未改，替换 replace 后可继续用。amd64 未构建。
- 目录用本地测试密钥签名（`/Volumes/500G/t5-stack/keys/`，不入库），正式信任根和 GitHub 签名本期不做。验签通过：`catalog vt5-1 verified: http-adapter@1.2.0-identity, homeassistant@1.0.0-t5`。

## 没做 / 注意
- 后端用的是 T4 worktree（`claude/wp-t4-plugin-identity-3cf9d2`）构建，没有用合入 develop（563e2497）后的代码重测；PG 迁移 `sql/33.sql` 后端启动时**不会**自动执行，回归时手工 `psql < 33.sql`（T4 文档已说明由安装/升级流程执行，目前仍缺这一环）。
- 没有真实 Home Assistant：HA 插件用假地址/假令牌，只验证了健康检查和平台调用；HA 数据打通属 T7。
- 心跳不续租：身份 ≤24h，到期前必须再次 `install`（新代次）才能续；`renew` 只延长 Agent 租约，不续身份（后续工作）。
- 回归没有走 `--device` 路径（它依赖 Broker 重载，本次最小栈没有 broker-owner）。
- 只在 arm64 验证；Docker 日志驱动为 none，无法读取插件容器日志，插件侧靠健康检查与后端访问日志证明。
- 事故：排查构建卡死时我执行过 `pkill -f "go build ./..."`，可能误杀其他会话同时运行的 `go build ./...`；其他会话如遇到异常中断，请重跑。

## 给 T7（HA 端到端）的用法
1. 起平台栈（需含 T4 后端、`plugin_runtime_identities` 表已迁移、Broker/MQTT 账号写入栈 `.env` 的 `TP_PROTOCOL_MQTT_USER/PASSWORD`）。
2. 镜像：本机需有 `sha256:f7a78a7e…`（见上）；签名目录用 `tools/plugin_catalog.py publish --key <测试密钥> --out DIR --version N --plugin-file plugins/homeassistant.json`。
3. 安装（HA 令牌不要写命令行）：
   ```
   TP_TENANT_EMAIL=... TP_TENANT_PASSWORD=... HA_ACCESS_TOKEN=... \
   python3 tools/plugin_install.py install --catalog DIR/plugin-catalog.json --trust-root trust-root.pem \
     --plugin homeassistant --set HA_BASE_URL=http://<ha>:8123 --stack-dir <栈目录> --web-url http://127.0.0.1:<端口> \
     --agent-dir <agent 目录> --agent-bin tp-agent --agentctl-bin tp-agentctl
   ```
   终端运行时缺 `HA_*` 会提示输入，secret 不回显；脚本里加 `--non-interactive`。
4. 卸载：同样参数换 `uninstall --plugin homeassistant`（不需要 catalog/agent-bin）。重装自动使用更高代次。
5. 升级/续身份：再次 `install`。
6. 若出现 409「平台已有 ≥N 代次」：Agent 状态文件丢失，需要恢复 `<agent-dir>/plugins/homeassistant.json` 或在后端侧清理。
