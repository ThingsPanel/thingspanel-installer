# 正式安装入口的配置更新与恢复边界

## 使用方式

原有 `install.sh` / `install.ps1` 一行安装入口保持不变。`install.bash`、`install.core.ps1`、`upgrade.sh`、`upgrade.ps1` 调用同一个 Python 标准库实现 `update.py`；它是现有安装器的内部模块，不是另一套部署入口。需要 Python 3.8 或更新版本；需要创建 native TLS 证书时，沿用 `prepare.py` 的 OpenSSL 依赖，并在配置替换之前检查该命令存在。Windows 仍使用原正式 Compose，服务初始化与 POSIX 采用同一 native 流程。移除了 Windows 安装中的 `compose down --remove-orphans`，正常更新不会清理已有栈。

macOS pkg 和 Windows Inno 包只落地 bootstrap/install 入口及原有 UI launcher，不在脚本启动前直接复制 Compose、native 配置或 upgrade/uninstall。它们同样交给上述事务提交；macOS postinstall 也只修改其安装入口的执行权限，不先修改旧管理脚本。发布构建依赖三平台事务测试，避免打包路径绕过入口保护。

四个入口固定校验 updater 的 SHA-256。本地对应文件摘要相符时直接使用；否则先在临时目录完整下载并核对，摘要不符立即退出。发布时必须同时发布 updater、四个入口和其余负载。升级脚本声明的 updater 摘要必须与本次受信任 updater 相符，否则拒绝提交配置。HTTPS 分发仍是负载内容的信任来源；本改动没有声称实现插件制品签名或独立的发布包签名体系。

## 下载、提交和激活

每次操作在安装目录创建私有 `.thingspanel-update`。同一内核文件锁覆盖旧状态恢复、备份、下载、校验、替换及 native 激活全过程。进程退出或被杀后内核自动释放锁；不按 PID 删除所谓陈旧锁，也不允许第二个安装过程越过锁。

替换组包含：正式 `docker-compose.yml`、`niginx.conf`、5 个既有 native 文件、当前平台的 upgrade/uninstall 脚本，以及经过入口摘要校验的 updater。全部远端文件下载并校验完成前，不修改原文件内容或模式。原文件完整备份；原来不存在的文件记录为空状态。`.env`、`plugin-token` 作为受监视输入保留，不进入下载覆盖组。目录、文件和 journal 使用写入刷新及同目录原子替换；恢复保留原 POSIX mode/uid/gid。

可信 updater 自己解析 JSON、Python 语法、Compose 和 PG/unknown 守卫，不导入或运行下载中的 `prepare.py`。暂存 Compose 通过原安装目录作为 `--project-directory`、原 `.env` 作为 `--env-file` 校验；传入的 `COMPOSE_FILE` 等不能把 native 子进程引到另一套配置。显式指定组件版本时，还要求暂存 Compose 的 backend 镜像 tag 相符，不会下载当前配置却宣称安装了另一个版本。

每次替换前再次比较原文件与预检快照；`.env`、token 或待替换文件变化会停止更新。激活前再次确认整个替换组仍是已校验的新内容。不要在安装过程内另开不遵守锁的配置编辑器；检查可发现预检后的修改，但不能把普通文件系统的检查与替换宣称为对任意外部写者的原子比较交换。

| journal 阶段 | 含义 | 下次运行 |
| --- | --- | --- |
| `prepared` / `applying` | 尚未激活；可能已替换部分文件 | 先恢复原组，再开始新操作 |
| `config_committed` | 配置已替换，尚未开始 native 激活 | 仍可恢复原组 |
| `recovering` | 配置恢复中途被中断 | 校验备份与当前内容，继续幂等恢复 |
| `activation_started` | 已准备调用 native 配置、迁移或服务操作 | 阻止自动更新/恢复，保留现场，交由负责人核对数据库和服务状态 |
| `complete` / `recovered` | 操作完成或配置已恢复 | 允许下一次操作 |

`activation_started` 在调用 native 代码之前持久化，因此即使激活尚未发生、进程恰好被杀，也会保守要求复核。失败后不能删除 journal 来“重试”，也不能称恢复 Compose 为数据库或业务回滚。原 `prepare.py` 的配置生成、pull、bootstrap、refresh、compose up、verify 顺序继续由同一个持锁进程调用；外部效果的协调恢复属于单独的迁移/业务收尾流程。

## 仅恢复尚未激活的配置

正常 install/upgrade 会先尝试处理前次未完成的配置事务。也可以用同版本、已审阅的 updater 显式恢复，命令不会下载文件或启动服务：

```sh
python3 /path/to/reviewed/config/native/update.py --root /opt/thingspanel --recover-config-only
```

```powershell
python C:\reviewed-installer\config\native\update.py --root C:\ThingsPanel --recover-config-only
```

恢复前先检查整个文件组。当前文件只允许与记录中的旧版本或新版本完全一致；一旦发现人工修改、符号链接、损坏 journal、缺失或变化的备份，停止恢复并保留现场，不自动覆盖第三方内容。原来不存在的文件只在内容/模式与本次写入一致时删除；新增目录只尝试删除空目录，不递归清理。恢复本身再次被中断，也可再次执行同一命令。

`.thingspanel-update` 可能含旧配置与凭据，不得提交、公开上传或作为无脱敏附件发送。本实现不自动清理历史操作备份，以免删除故障恢复证据；已完成记录的保留期管理另行处理。支持范围是本机文件系统上的单安装目录；不得假定网络共享目录或存储设备掉电行为等同于本机文件系统。Windows 代码使用系统文件锁和写穿替换，但本轮 Linux 测试不构成 Windows/NTFS 或真实 PowerShell 安装验收。

## ThingsVis 身份与 activation 的 `.env` 记录

`prepare.py` 为部署生成并持久保留 `TP_THINGSVIS_AUTH_SECRET`，以及三份相互独立的安装 provider、ThingsVis identity、ThingsVis import 服务凭证。重复 prepare 和通常升级复用已保存值；环境覆盖与已有服务凭证不一致时拒绝，不打印凭据。broker 的只读 JSON 使用同一 updater 的原子替换工具写入，避免非 root 的第二次 prepare 因旧文件为 `0440` 而失败。

安全 SSO 切换必须由发布负责人显式选择 `TP_THINGSVIS_IMAGE` 并设置 `TP_THINGSVIS_AUTH_POLICY_TARGET=verified-sso-v1`，所选制品需对应已验收的源码和镜像锁。默认 `v1.0.23` 不能取得此标记。首次跨越该策略边界会轮换 AUTH_SECRET，即使旧值很强也不能复用，因为旧 accept-all SSO 可以签发不可信会话；用户需重新登录。新 secret 与 `TP_THINGSVIS_AUTH_POLICY=verified-sso-v1` 同一次原子 `.env` 写入，之后重复运行保留该身份。未知 policy、丢失的已受保护身份及隐式策略降级均拒绝。

标记仅记录该次配置意图，不证明容器已经切换。`.env` 在配置 commit 前仍是不可修改的 watched input；activation 时，同一个 journal 的 `activation_files` 先记录原 `.env` 备份、原/新摘要和未完成意图，再原子替换，最后记录 `applied`。完成外层事务会保留这份回执。若意图或文件替换后进程被杀，journal 仍为 `activation_started`，恢复会要求核对实际服务，不猜测旧/新 secret 哪个已生效，也不自动还原数据库或会话状态。

broker 的 `plugin-token`、`owner-tokens.json`、`services.json` 迁入最小专属目录 `config/native/broker`，Compose 只将该目录挂到 `/run/secrets`。逐文件 bind mount 会固定旧 inode，而目录挂载允许原子替换后的新文件被再次打开读取。首次迁移保留旧 token、owner 列表与 services 数据，旧路径文件保持原样；身份不一致时拒绝。三份新文件的原不存在状态、旧/新摘要和新建目录意图同样进入 `activation_files` / `activation_dirs`。旧容器须由同一正式 Compose 变更受控重建；配置生成成功不等于原进程已经采用新挂载。

只有当前持锁 updater 传入相同操作标识的 prepare 子进程可继续 activation；普通 prepare 不能以内容已经相同为由绕过未知安装状态。该标识用于本机进程关联，不是远端身份或安全围栏。已完成安装之后，原有显式 `--refresh-devices` 仍可在同一内核锁下原子更新 `broker/services.json` 这一资源投影；它不更换身份、不重开安装操作、不把未知安装改成成功。

`TP_THINGSVIS_IMAGE` 支持隔离验收使用的本地镜像选择；本机 image ID 不是远程 registry manifest digest。当前 formal release 仍缺少具有独立信任根的整组签名 manifest；仅 HTTPS 和 updater hash 不证明全部 payload 的发布身份。旧已安装入口对 updater 的固定 pin 也不会自动授权不同字节的新 updater。此类跨 updater 版本升级必须经已审阅的新 bootstrap 或补齐正式签名发布协议，不能以放宽 hash 检查自动重试。

隔离环境使用已有本地制品时，另设置 `TP_THINGSVIS_LOCAL_IMAGE_ID` 为完整 Docker `sha256:...` image ID。native pull 先核对该 ID 与 Compose 实际选择一致，然后只对这一 ThingsVis 镜像跳过 registry pull；其他组件保持原规则。ID 不符或镜像缺失立即失败，不回退下载另一个版本。生产 registry 引用应使用发布清单对应的远程摘要，不能把本机 image ID 填成 registry digest。

## 发布源、信任根与升级辅助镜像（R9）

平台内“检查更新 / 升级”由宿主 Agent 的 system 角色执行。它读取一个**签名发布清单**（`<发布源>/release-manifest.json` + `.sig`），只信任一把由运维**带外**提供的 ECDSA P-256 公钥。安装/升级入口把下面几项传给 updater，updater 再传给 `agent_host.py up --system`（每次 install/upgrade 都会重跑，幂等；`system.json` 由当前 Agent 二进制重新生成，已有 token 保留）。

| 设置 | 入口环境变量 | 说明 |
| --- | --- | --- |
| 发布源 | `TP_RELEASE_SOURCE`（或 `.env` 同名键） | `https://` 基础 URL。**没有默认值**：未配置时 Agent 没有 upgrade 配置，“检查更新”返回 `updates_not_configured`（界面应显示“未配置发布源”）。 |
| 信任根 | `TP_RELEASE_TRUST_ROOT`（绝对路径，或 `.env` 同名键） | 与安装包签名校验共用同一个变量：设置它还会要求本次下载的安装包由该密钥签名。必须在安装目录之外，必须是 P-256 公钥 PEM。 |
| 额外 CA | `TP_RELEASE_CA` | 私有镜像站的 CA 包（可选）。 |
| 辅助镜像 | `TP_UPGRADE_HELPER_IMAGE` | 升级时运行 `docker compose` 的镜像，默认 `docker:27-cli`。 |
| 更换信任根 | `TP_REPLACE_RELEASE_TRUST_ROOT=1` | 见下。 |

优先级：入口参数/环境变量 > 安装目录 `.env` > 上次保存的值。保存位置 `.tp-agent/release/`（0700）：`settings.json`、信任根副本 `release-trust-root.pem`、CA 副本。

**信任根的钉扎**：信任根从不取自发布源或下载内容。updater 在配置事务**之前**校验并复制它，并记录 SHA-256。之后：

* 再次提供的文件与记录不一致 → 拒绝（不改任何东西），除非显式设置 `TP_REPLACE_RELEASE_TRUST_ROOT=1`；
* 磁盘上的副本与记录不一致（被改动）→ 拒绝重新配置，并且 Agent 启动时**不启用升级**（平台和插件照常）；
* 设置了发布源但没有信任根 → 升级保持关闭；
* 正式信任根将作为常量 `OFFICIAL_RELEASE_TRUST_PEM` 写进 `update.py`（与现有 `PLUGIN_TRUST_PEM` 同样处理，随入口对 updater 的摘要固定一起受保护）。目前为空，只有运维提供的测试信任根可用。有了该常量后，没有显式提供信任根的机器自动使用它；beta 机器切换：`TP_REPLACE_RELEASE_TRUST_ROOT=1` 且不提供 `TP_RELEASE_TRUST_ROOT`，再运行 `upgrade.sh`（或提供正式公钥文件）。

**升级辅助镜像**：install/upgrade 在启动 Agent 前确认本机有该镜像，没有就拉取；拉不到只警告并打印 `docker save/load` 办法。升级预检再检查一次：缺失且拉取失败时返回 `upgrade_helper_image_unavailable`，此时尚未快照、停服或替换任何文件，Agent 日志里有离线导入步骤（`UPGRADE-HELPER-IMAGE`）。

**helper 摘要**：`agent_host.py` 等受钉扎的 helper 每次修改都要在 `PLUGIN_HELPER_PROFILES` 最前面加新的 profile（旧的保留），并重新计算入口脚本里的 `UPDATE_ENGINE_SHA256`。发布前运行 `tests/test_update_transaction.py`（含入口摘要检查）和 `tests/test_plugin_bundle_activation.py`。
