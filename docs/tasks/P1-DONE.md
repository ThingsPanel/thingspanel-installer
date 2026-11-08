# P1 — HA 接入剩下的机器步骤全部自动化

**结论：在新栈上，只做“一条命令安装”和“在平台上选 HA 实体建设备（API 模拟界面）”，没有执行任何手工命令，上行、下行都通过。** 未推送、未合并、未发布镜像。

- 仓库：安装器；分支 `wp/p1-auto-steps`（从 develop `4470874` 切出）；worktree `/Users/junhong/Downloads/code/thingspanel-installer-wp-p1`。
- 提交：`7b97e9d`（功能与测试），另有一个提交含本说明和输出行微调（见 `git log wp/p1-auto-steps`）。
- 证据（不进仓库）：`/Volumes/500G/archive/evidence/2026-11-04-P1/`（已 grep：令牌和平台密码为 0 命中）。

## 1. 做了什么

| 要求 | 实现 |
| --- | --- |
| 自动执行 `sql/33.sql` | `config/native/prepare.py` 的 `bootstrap()` 在 sys_version 循环之后，把后端镜像里的 `sql/33.sql` 当作独立组件 `community-ext` 记账执行（校验和台账，重复执行跳过，文件被改动则拒绝）。**原因**：后端 `VERSION_NUMBER=25`，26–32 由后端启动时自己执行（内嵌），33 没人执行，所以 T7b 要手工跑。旧镜像没有这个文件则跳过。`apply_migrations` 的文件号解析也改成能读 `33.sql`（原来只读 `001_x.sql`）。 |
| 自动执行 `sql/broker/001–017` | 本来就在 `bootstrap()` 里（`broker` 组件）。本卡验证：新栈上 `--bootstrap` 一次到位，17 条记账；重装不重复。安装和升级（`update.py` 的 `activate_native`、`reconcile_activation`）本来就调用 `--bootstrap --refresh-devices`，不需要改入口。 |
| 修正 `--refresh-devices` | 新增纯函数 `scope_services(env, targets, plugins)`：平台账号 `tp-community-platform` 的 `allowed_device_ids/allowed_tenant_ids` = 普通 MQTT 设备 ∪ 插件设备（去重），`publish` 因此非空；协议账号仍只含插件设备。`refresh()` 与新的 `sync_scope()` 共用它，范围只有一个定义。 |
| 新增/删除插件设备后自动刷新并按序重启 | 见 §2。 |
| 单元测试 | `tests/test_p1_auto_steps.py` 20 项，见 §3。 |

## 2. 触发方式（设计）

**选择：安装器命令 + 宿主机上的小监视进程**，没有放进 Agent。

- `prepare.py --sync-scope`：查询平台设备 → 算出期望范围 → 与现有 `services.json` 比较；**没变化什么也不做**；变化则写文件，然后按 **broker-owner → mosquitto** 的顺序 `docker compose restart`（mosquitto 与 owner 共网络命名空间，必须后重启）。新增和删除设备都是“范围变化”。
- `tools/scope_sync.py`：`once`（跑一遍）、`watch`（每 20 秒一遍，前台）、`ensure`（没有监视进程就起一个脱离终端的）、`stop`。`watch` 只在结果变化时写日志（`<agent-dir>/scope-sync.log`），不含凭据。单次失败（例如安装器正持有更新锁）下一轮重试，不退出。
- `plugin_install.py install` 结束时先 `once`（失败则报错），再 `ensure`；`renew`（本来就要放进 cron）也调用 `ensure`，**机器重启后下一次 renew 会把监视进程拉起来**。
- 为什么不放 Agent：Agent 是 Go 容器，没有数据库访问和 compose 重启能力，要加 PG 驱动和 compose 权限；范围逻辑本来就在 `prepare.py`（Python），放在这里只有一份实现。代价见 §5。
- 反应时间（实测）：创建设备后 **约 12 秒** 范围刷新并重启；插件自己每 60 秒同步设备，所以新设备最长约 80 秒后可用。

## 3. 测试

- 新增 `tests/test_p1_auto_steps.py` 20 项全过：平台范围含插件设备；只有插件设备时 `publish` 仍非空（T7b 发现 3 的回归）；无设备时为空范围；去重；协议账号不含 MQTT 设备；范围不变不写不重启；新增/删除都触发；首次无文件；写文件失败时不重启；`--refresh-devices` 同口径；`33.sql` 记账一次、重复跳过、被改动则拒绝；`bootstrap()` 用假 Docker 走一遍，33.sql 在 broker 之前执行，旧镜像无该文件时跳过；监视器 `run_once` 不抛异常、`watch` 失败后继续且只记变化、`ensure` 幂等且不被回收的 pid 欺骗。
- 回归：`tests/` 全部 107 项 OK（含新增的）；`tools/test_plugin_install.py` 18 项 OK；`config/native` 下三个测试文件 21 项 OK。
- **这些是单元测试，不是端到端验收**；端到端见 §4。

## 4. 验收（真实栈，容器名 `p1-stack-*`、`p1-homeassistant`）

环境（验收脚手架，不是产品步骤）：
- 栈用本分支的 `docker-compose.yml`（项目名 `p1-stack`），`docker-compose.override.yml` 只把 broker-owner/mosquitto 换成本机 develop 构建的 `thingspanel-mosquitto:t7b-local`（发行的 v1.3.0 镜像仍过旧，见 §5）。只起 postgres、redis、broker-owner、mosquitto、backend（前端、encore、notification 没起，不影响本链路）。
- 后端：develop `09fe900a` 交叉编译 linux/arm64，叠在 v1.3.0 镜像上成 `thingspanel-go:p1-local`（带 develop 的 `sql/`、各 `migrations/`、`global.go`，`bootstrap()` 从这里读）。
- demo HA：`ghcr.io/home-assistant/home-assistant:stable` + demo 集成；本卡自己 onboarding 并建了长期令牌，结束后随容器删除。插件镜像 `thingspanel-homeassistant:p1-arm64`（HA 仓库 `wp/t6-ha-package` `5394d9c`，主机交叉编译；依赖本地 SDK replace）。目录用测试密钥签名。
- “一条命令” = `/Volumes/500G/p1-stack/p1-install.sh`：与安装器 `activate_native` 同样的调用（`prepare.py`、`--bootstrap --refresh-devices`、`compose up`）加 `plugin_install.py install`。**没有跑 `install.bash`**（它从线上拉发布的镜像，develop 镜像未发布）。

| 步 | 结果 | 证据 |
| --- | --- | --- |
| 1 一条命令 | 退出码 0，约 90 秒：`33.sql` 与 17 个 broker 迁移自动记账（`community-ext|33`）；服务插件自动登记；generation 1，健康检查（含稳定别名）ok；监视进程已起 | `step1-install.log`、`migration-ledger.txt` |
| 2 平台上选实体建设备（API 模拟 UI） | 建接入点，列出 33 个 HA 实体，选 Bed Light 和 Outside Temperature 批量建设备 | `step2-create-devices.log` |
| （无人干预）范围刷新 | 19:52:52 建设备，19:53:04 监视进程发现变化，写入 `services.json`（平台账号 2 台设备、`publish` 12 条），重启 owner → mosquitto；backend 和插件自动重连，**没有重启插件和后端** | `scope-sync.log`、`mosquitto.log`、`services-scope-summary.txt` |
| 3 上行 | HA 里改温度，平台当前值 `ha_state` = 新值；灯的状态/亮度等多项属性入库 | `step3-uplink.log` |
| 4 下行 | 平台下发 `switch` false→true→false，HA 灯 off/on/off，三次回执 `result:0`、状态 3 | `step4-downlink.log` |
| 5 新增 + 删除设备 | 加 Decorative Lights、删 Outside Temperature：约 12 秒后范围换成 {Bed Light, Decorative Lights}，owner → mosquitto 按序重启 | `step5-add-remove.log`、`scope-sync.log` |
| 6 新设备下行/上行 | 第一条命令在插件同步到设备之前发出：**平台显示失败**（状态 4，`device number … is not bound in connector`），命令确实到达了插件，说明范围是对的；约 40 秒后下发 false/true 成功（状态 3，`result:0`），新设备有 `ha_state` 历史 | `step6-new-device-downlink.log` |
| 7 重装 | 再执行一次同一条命令：退出码 0，generation 2，`community-ext` 仍 1 行，监视进程复用（`running`），只有 1 个插件容器 | `step7-reinstall.log` |

收尾：`p1-stack-*` 容器、卷、网络、`p1-homeassistant`、`tp-agent-p1-stack`、两个 `tp-agent-prepared-*` 容器已删（只含本卡创建的）；监视进程已停；HA 令牌、`.env`、broker 凭据、HA 配置已从栈目录删除。栈目录 `/Volumes/500G/p1-stack/`（脚本、二进制、目录、构建物）留给审查，可整个删除。

## 5. 没做 / 局限 / 需要知道

- **监视进程不是系统服务**：机器重启后它不会自己回来，直到下一次 `plugin_install.py renew`（cron）或再次 `install`。要彻底解决需要 launchd/systemd 单元或把逻辑放进长期运行的容器，未做（要先定平台范围）。如果监视进程停了，新设备的下行会被 Broker 静默丢弃——这正是 P3（后端让命令超时失败可见）要补的另一半。
- **发行版 Broker 镜像仍须重建**：验收用的是本机的 `thingspanel-mosquitto:t7b-local`；`docker-compose.yml` 里仍写 v1.3.0，新栈用发行镜像仍会 CONNACK 5（P4 发布镜像后改引用）。同理 HA 插件目录里的镜像 ID（`plugins/homeassistant.json`）指向已删的 `t7c` 镜像，验收用的目录是临时重签的。
- 范围上限沿用原来的：普通设备 ≤256、插件设备 ≤256；HA 实体多于 256 台设备会报错，需另设计多身份。
- 新设备从创建到可用最长约 80 秒（监视 20 秒 + 插件同步 60 秒）；期间下发命令会如实显示失败。
- 监视进程每 20 秒执行一次 `prepare.py`（会跑 `docker compose config` 和一次查询），开销小但不是零；未做事件驱动。
- 未改后端（`33.sql` 未被后端自己执行是后端侧的设计，若要改应由后端在启动时执行并记账）；未改 Agent；未跑 Windows 路径。
- `update.py` 在升级中途持有更新锁时，监视进程的那一轮会失败并在下一轮重试（单元测试覆盖了“失败不退出”，没有在真实升级中演练）。
- Agent 创建的插件容器名 `tp-agent-prepared-*` 是代码固定的，没有 `p1-` 前缀（同 T7c 记录）。
