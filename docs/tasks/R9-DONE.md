# R9 交付说明：安装器的发布源/信任根、Agent 刷新、辅助镜像、插件目录、beta 版本号

- 分支：`wp/R9-installer-release`（从 `base/origin-develop-20261113` 切出，未推送、未合并、未发布）
- worktree：`/Users/junhong/Downloads/code/thingspanel-installer/.claude/worktrees/wp-r9-installer-release-58f2ec`
- 配套：[beta 发布与测试操作清单](R9-BETA-RUNBOOK.md)、`config/native/UPDATE-SAFETY.md`（新增“发布源、信任根与升级辅助镜像”一节）
- 证据（不在仓库）：`/Volumes/KIOXIA/archive/evidence/2026-11-13-R9/`（脚本 + 日志）

```
cd39dcc docs: release source, trust root pinning and helper image (UPDATE-SAFETY)
121f283 tp-agentctl: system updates / check subcommands
7b53b17 prepare.py: same fresh-install fix for the relational password guard (R9/B6 finding)
c10910a installer: a fresh install (no .env yet) passes the PostgreSQL guard ... (R9/B6 finding)
59bd7d6 installer: plugin catalog sync from the release source ... re-pin helpers and updater (R9/B4)
e0c6ea6 installer: release source, pinned trust root and upgrade helper image flow ... RAW_BASE override (R9/B3,B5,B7)
229683d agent: configurable upgrade helper image, precise offline error from the preflight (R9/B7 agent side)
2d52e96 agent: order vX.Y.Z-beta.N versions ... tests for restore, upgrade, update check (R9/B8)
```
（以 `git log --oneline base/origin-develop-20261113..HEAD` 为准；本文件所在提交在其后。）

## 做了什么

### B8 beta 版本号（`agent/restore.go` `CompareVersions`）
- 规则：`vX.Y.Z[-beta.N][+build]`。数字字段逐段按数值比较；同号时 `beta.1 < beta.2 < beta.10 < 正式版`；`v1.3.x < v1.4.0-beta.1`；`+build` 忽略。
- **其他后缀（`-rc.1`、`-dev`、`-dirty` 等）不再解析**：只能字符串相等，否则 `ok=false`（恢复/升级拒绝“无法判断”）。这是对旧行为（后缀被忽略、只比 X.Y.Z）的**收紧**：以前 `v1.4.0-dev` 与 `v1.3.0` 能比较，现在不能。
- 恢复兼容、升级路径、更新检查三处都用它；`upgradeVersionPattern` 与备份名正则本来就允许 `-beta.N`，无需改。
- 前面三处都有单元测试（`agent/version_beta_test.go`）：beta→更高 beta、beta→正式、旧正式→beta 允许；同版本、降级、正式→同号 beta 拒绝；beta 备份恢复到正式机器允许、正式备份恢复到同号 beta 机器拒绝、beta.2 备份到 beta.1 机器拒绝；检查更新的 `newer` 对 beta 正确。
- （d）检查：安装器仓库 Python 侧没有任何版本号比较。后端 `encore-api/system/api.go:26` 的 `versionPattern` 已接受 `-beta.N`；界面“已是最新”的判断是服务端给的（`UpdateAvail = Agent 的 Newer`），所以由 Agent 的规则决定。**没有检查的**：前端 Vue 组件如何显示版本字符串（我在前端 `develop` 里没找到更新对话框组件，没法确认）；后端 `internal/` 里其它按 `vX.Y.Z` 解析版本的地方（我只读了安装器仓库能确认的范围，另按后端 `origin/develop` 做了 `grep`，只命中上面那一处）。

### B3 发布源与信任根
- `update.py` 新参数 `--release-source/--release-ca/--upgrade-helper-image/--replace-release-trust-root`；四个入口脚本（`install.bash`、`upgrade.sh`、`install.core.ps1`、`upgrade.ps1`）从 `TP_RELEASE_SOURCE`、`TP_RELEASE_CA`、`TP_UPGRADE_HELPER_IMAGE`、`TP_REPLACE_RELEASE_TRUST_ROOT=1` 转成参数（sudo 会丢环境变量，所以用参数）。信任根沿用已有的 `TP_RELEASE_TRUST_ROOT`。`.env` 同名键为较低优先级的备选，上次保存的值最后。**没有写死任何 URL**。
- 保存到 `<安装目录>/.tp-agent/release/`（0700）：`settings.json`、信任根副本、CA 副本，记录 SHA-256。信任根只来自带外文件（必须在安装目录外、P-256 公钥）；换成不同文件必须 `TP_REPLACE_RELEASE_TRUST_ROOT=1`；磁盘副本被改动 → 拒绝重新配置，且 Agent 启动时不启用升级。配置事务**之前**校验，失败则什么都不改。
- 没配置发布源：Agent 无 upgrade 配置，`system updates` 返回 `updates_not_configured`（"update check is not enabled"）——真实验收里见到。
- 正式信任根：`update.py` 里预留常量 `OFFICIAL_RELEASE_TRUST_PEM`（目前为空）。切换办法见 runbook §7。
- 新增一个入口环境变量 `RAW_BASE`：`install.bash`/`upgrade.sh`/两个 ps1 现在与 `install.sh` 一样允许覆盖安装包来源（默认值不变）。**没有它，beta 无法从自己的发布源安装**。
- 受钉扎 helper 的摘要：已有的 `PLUGIN_HELPER_PROFILES` **在开始前就与仓库不符**（`plugin_catalog.py` 被后来的提交改过、`agent_host.py` 被 R6 改过都没更新钉），导致 `tests/test_plugin_bundle_activation.py` 7 个测试失败，**也意味着照仓库当前内容发布的安装包无法暂存插件 bundle（Agent 起不来）**。本卡在 `agent_host.py` 之后新增了最前面的 profile（当前文件摘要）并更新四个入口的 `UPDATE_ENGINE_SHA256`；所有 187 个 Python 测试通过（之前 7 个失败）。以后每次改这些文件都要重钉（见 UPDATE-SAFETY）。

### B5 升级 Agent 后自动刷新配置
install/upgrade 每次都在 `start_plugin_agent` 里重跑 `agent_host.py up --system`（已有 token 保留），所以只要 bundle 里的 Agent 换了，`system.json` 就由新二进制重新生成。本卡让这一步带上发布源/信任根/辅助镜像。真实验收见下。**限制**：现行流程只在“没有 active bundle”时下载 bundle（且 bundle 的 catalog/Agent/HA 镜像地址写死为官方站和 GitHub Release，不跟 `RAW_BASE` 走），所以没有任何命令能把 active bundle 里的 Agent 升到新版本——见“需要负责人决定”。

### B7 升级辅助镜像
- 名字可配置（`TP_UPGRADE_HELPER_IMAGE`，默认 `docker:27-cli`），经 `agent_host.py --compose-image` → `tp-agent -compose-image` → `system.json`；格式校验（无空格/shell 字符）。
- install/upgrade 在启动 Agent 前检查本机是否有该镜像，没有则拉取；拉不到只警告并打印 `docker save/load` 办法。
- 升级预检：缺失且拉不到 → 新错误码 `upgrade_helper_image_unavailable`（预检阶段，未快照、未停服、未替换），Agent 日志 `UPGRADE-HELPER-IMAGE:` 一行里有离线导入步骤。**界面上这个错误码如何显示取决于前端/Encore 的码表——未检查，见下。**

### B4 插件目录与镜像（最小方案）
`python3 <安装目录>/config/native/update.py --root <安装目录> --sync-plugin-catalog [--image-dir DIR] [--allow-missing-images]`（实现在受钉扎的 `agent_host.py catalog-sync`）：从 `<TP_RELEASE_SOURCE>/plugins/catalog-linux-<arch>.json(.sig)` 取目录，用插件信任根验签（Agent 之后还会再验），要求是“可直接替换”（Agent 二进制与 HA 镜像包摘要不变、版本不回退、平台一致、未过期），逐条目确认镜像在本机，缺的从 `--image-dir/<id>-linux-<arch>.tar` 导入（校验归档摘要和镜像 ID），任一缺失则不替换目录。有单元测试（`tests/test_catalog_sync.py`，用真实签名，Docker 调用被替身替代）。**没有真实验收**。

## 怎么验证的

### 单元测试（不是整栈验收）
- Go（Linux 容器内 `go test ./...`）：全过。新增 `version_beta_test.go`、`helper_image_test.go`。
- Python（`python3 -m unittest discover -s tests`）：187 个全过；新增 `test_release_settings.py`（19 个）、`test_catalog_sync.py`（6 个）。`tools/` 下有 6 个与本卡无关的失败（`test_plugin_install.UserEnv`）在基线上就存在。

### 真实验收（隔离 Docker 项目 `r9`，macOS arm64 / OrbStack；结束后已清理自己创建的容器、卷、网络、`r9-*` 镜像与工作目录）
用的是真正的 `install.bash` 与安装后的 `upgrade.sh`；后端镜像用后端 `origin/develop` 源码加 `SYSTEM_VERSION=vX.Y.Z[-beta.N]` 本地构建（`v1.3.0`、`v1.4.0-beta.1/2`、故意迁移出错的 `v1.4.0-beta.3`、`v1.4.0`，每个带自己的迁移 SQL 和 `VERSION_NUMBER`），Encore 镜像按 `build-encore-image.sh` 的服务清单（含 `system`）用 `origin/develop` 构建，前端用此前演练的 UI 镜像；发布清单用测试密钥签名，发布源是本机 nginx（安装器要求 HTTPS，Agent 通过自签 CA 访问）。
| 验收项 | 结果 |
| --- | --- |
| 全新安装 v1.3.0（`install.bash`，旧 Agent bundle，只给信任根不给发布源） | **通过**（修了两处阻塞 bug 后，见“发现的问题”1）。`system status` 全部 ok；`system updates` → `updates_not_configured`；`system.json` 无 upgrade 块 |
| 配置发布源并刷新 Agent（`upgrade.sh v1.3.0` + 新 Agent bundle） | **通过**：Agent 二进制 `4388…→9fa6…`、容器重建、`system.json` 重新生成并出现 upgrade 块、system token 不变（`9dc0…`）、辅助镜像被拉取 |
| 检查更新 | v1.3.0 机器：服务 beta.1 → `newer:true`；beta.2 机器：服务 beta.2 → `newer:false`；服务 beta.1 → `newer:false`；服务 beta.3 → `true`；服务正式版 → `true`；正式版机器 → `newer:false` |
| 升级 v1.3.0 → beta.1 → beta.2 → v1.4.0 | **均通过**（36–42 秒），`sys_version` 26/27/28，迁移标记行 1 / 1,2 / 1,2,3，设备数、用户数、自建 50 行数据全程不变 |
| 降级尝试（beta.2 机器，服务 beta.1） | `upgrade_not_newer`，预检阶段拒绝，站点不变 |
| 故意失败的升级（`v1.4.0-beta.3`，迁移 `SELECT 1/0`） | **自动回退**：`upgrade_failed_rolled_back`，回到 beta.2、`sys_version` 27、标记行 1,2、半成品表不存在、数据不变；耗时 339 秒（健康门等满，已知局限） |
| 辅助镜像不可用（配成不可拉取的名字，再试升级） | 预检返回 `upgrade_helper_image_unavailable`，Agent 日志有离线导入步骤，站点除状态行外完全相同；install/upgrade 阶段也打印了警告和 `docker save/load` 办法 |
| 恢复：beta.1 备份恢复到 v1.4.0 机器 | **未通过也未能判定**：恢复自动回退（`restore_failed_rolled_back`），站点未变。**很可能是验收镜像的人为问题**（我构造的 `v1.4.0` 镜像只带自己的 `28.sql`，没有 `27.sql`，旧备份向前迁移缺文件），但我没有去证实。正式版/beta 备份兼容规则只有单元测试 |

没有做到的：
- **“全新安装并启用发布源”一次完成**没有单独跑一遍（我用“安装 → 带发布源再跑 `upgrade.sh`”代替；参数在两条路径上是同一套代码）。
- **bundle 不是走“首次下载”路径**：因为 `prepare_optional_plugin` 的 catalog/Agent/HA 镜像地址写死为官方站与 GitHub Release（官方 Agent 目前没有 system 角色），验收里预先放置了一个用测试插件密钥签名的 bundle（`current.json` 指针 + `TP_PLUGIN_TRUST_ROOT`），内含 HA 的占位镜像。这是现有“已有 bundle 的机器”路径，不等于“全新机器从官方站下载 bundle”。
- UI 没有点：只用 `tp-agentctl system status/verify/updates/upgrade/backup`（我给 `tp-agentctl` 补了 `updates`、`check` 子命令，之前没有）。
- `catalog-sync` 没有真实验收；macOS 之外的平台、Windows、断电、HA 真实插件、多架构均未测。

## 发现的问题（已修 / 待办）

1. **已修：全新安装根本过不了 updater 校验。** 没有 `.env` 时 Compose 无法插值，updater 与 `prepare.py` 的 PostgreSQL 守卫拿未插值的文件检查，`GOTP_RELATIONAL_POSTGRES_PLATFORM_PASSWORD` 的 `${TP_DATABASE_PASSWORD:-…}` 被当成“未解析”而拒绝（`Cannot confirm supported PostgreSQL backend…`）。已改为密码字段允许引用，端点字段仍须是字面值；两处都有回归测试（含用仓库真实 `docker-compose.yml`）。这是交接文档 B6“没在全新机器装过”的真实答案：**修之前装不上**。
2. **待办：安装器事务与 Agent 的 Broker 范围同步竞争 `config/native/broker/services.json`。** 创建设备后几秒内跑 `upgrade.sh`，updater 报 `Install configuration changed during update: config/native/broker/services.json` 并中止（配置未改，重试即可）。Agent 的范围同步每 20 秒可能改写这个被 updater 监视的文件。
3. **待办（后端/前端）**：新错误码 `upgrade_helper_image_unavailable` 需要在 Encore `system` 的错误映射和前端码表里加说明文字（我没改后端/前端，也没检查它们是否会把未知码显示成通用错误）。
4. `prepare.py --verify` 硬编码检查 `127.0.0.1:19091` 和 `:503`；同机有其他栈占用 503 时检查会假阳性。（验收里就是这样，不改。）
5. 升级失败回退最坏约 339 秒（已知，交接文档 §5.6）。

## 需要负责人决定

1. **正式版“不重新构建”做不到**：后端 `--version` 是编译期注入（`-X …SYSTEM_VERSION`），所以 beta 镜像直接加正式版标签后 `--version` 仍是 beta 号，Agent 会认为版本没变。要做到“同一批构建提升为正式版”，需要后端支持运行时提供版本号（例如环境变量覆盖 `--version`），属于后端改动；否则正式版必须重编后端二进制（其余镜像可复用）。runbook §1 已写明。
2. **Agent 如何随 beta 发到机器上**：现行 bundle 只在“没有 active bundle”时下载，地址写死为官方站/GitHub Release。要让 beta 带新 Agent，要么让 bundle 流程跟 `RAW_BASE`/发布源走并支持替换 active bundle，要么 beta 测试机手工放置 bundle。
3. **谁签发布清单、beta 与正式是否同一把钥匙**；`OFFICIAL_RELEASE_TRUST_PEM` 何时填。
4. **插件目录/镜像分发的约定**：发布源上的目录路径（我沿用现有 `plugins/catalog-linux-<arch>.json`）、非 HA 条目的镜像归档放哪里和怎么命名（目录条目只有 `image_id`，现有设计只为 HA 定义了归档地址）、目录要求新 Agent/新 HA 镜像时走什么流程、是否在 install/upgrade 里自动同步（现在是显式命令）。
5. 收紧后的版本规则是否接受（`-rc`、`-dev` 之类不再可比较）。
6. 是否要让 install/upgrade 对问题 2（services.json 竞争）自动重试。

## 没动的
后端与前端仓库（只读检查）；`/api/system/*` 返回格式没有需求变更，只需要上面第 3 条的码表。
