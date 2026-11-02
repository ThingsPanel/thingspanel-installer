# Installer 接管与配置恢复复验

## 精确基线与责任

- 输入仓库：`/Volumes/500G/code/thingspanel-takeover-20261102/installer`。
- 输入提交：`22d970e4b46d286a9f03a6f0a50a370725732160`。
- 原候选目录只读。修改由 installer 唯一写入窗口经总负责人校验 before SHA 后集成。
- 本报告只维护 installer 模块事实；共同门禁与制品清单由总负责人维护。

## 已确认结论

旧外置下载 harness 没有放入当前入口所固定摘要的 updater，因此第 2～7 个案例停在第一次 bootstrap 摘要拒绝；这不构成后续 payload 边界已经测试。仓库内既有 harness 已修正 bootstrap 前提。本轮又增加逐次 HTTPS 主机、下载 URL 与序号的断言，实际运行安装/升级各 7 个故障边界，并保留独立的 4 个 bootstrap 拒绝案例。没有降低产品摘要校验来适配旧测试。

既有整组事务实现可复用，保留原锁、journal、备份、原缺失状态、模式、混发拒绝和 activation unknown 守卫。本轮补充 SIGTERM、双进程进入配置阶段、缺文件 SIGKILL 恢复及完成/恢复状态重启用例。

新配置回归定位了原 `prepare.save` 的实际问题：第一次写入 broker JSON 后设为 `0440`，第二次 prepare 仍原地打开写入，普通用户会收到 PermissionError。现复用 `update.py` 的原子替换工具，不临时放宽旧文件权限，不建立第二套配置写入模块。

为使原子替换对容器可见，broker 三个专属文件使用最小目录挂载；保留所有旧路径文件，继承旧身份、owner 列表及服务范围。新文件与目录的缺失状态、写入意图进入同一 activation journal。真实 Docker 文件可见性验证入口为：

```sh
python3 tests/verify_broker_directory_mount.py --image <已在本机且已审阅的镜像>
```

脚本只创建一个带唯一 `tp-takeover-b-installer-mount-` 前缀、`--network none`、只读根文件系统的临时容器；使用私有目录，结束删除该容器；不会拉取镜像或触碰业务栈。它同时记录目录挂载与单文件挂载控制组的实际读值。该脚本本地仅做语法验证，实际 Docker 结果须由总负责人补充，不能写成已经验证真实 Broker reload。

ThingsVis 不再使用公开默认 AUTH_SECRET。每部署身份与三份独立内部服务 token 持久化并做冲突校验；正式 Compose 接入 provider、identity、import 与当前授权回调。明确选择已验收新镜像和 `verified-sso-v1` 目标后，旧 SSO 身份轮换一次，随后复用。secret 与策略标记在同一原子 `.env` 内容中，由同一 update journal 保存旧/新摘要与原文件备份；未决 activation 一律要求实际服务核对。

## 实际执行与证据边界

执行平台为云端 Linux、Python 3.12。导入的 46 个基线文件均按远端提供 SHA-256 校验后使用。原样基线 28 方法通过。补丁后的命令为：

```sh
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s tests -p 'test_*.py' -v
```

首包 49 个方法通过；配置覆盖/编码 followup 后为 51 个方法通过。包含原 14 个 payload 下载边界、4 个 bootstrap 拒绝、20 个替换前后异常、6 个配置阶段 SIGKILL、恢复再 SIGKILL、3 个 SIGTERM、2 个 activation-env SIGKILL、broker 新目录中断、并发锁及新身份配置用例。子边界不是额外的顶层测试数。证据由总负责人保存并与实际集成 commit 和补丁摘要绑定；本报告不代替该绑定。

策略首次轮换前若调用进程导出旧 AUTH_SECRET，会因 Compose 的环境覆盖优先级阻止新 `.env` 生效；现在在任何配置写入之前明确拒绝该覆盖。已有合法加引号的 `.env` secret 保留原编码，不去引号重写触发 `$` 插值；动态值/无法无歧义持久化的输入拒绝且不打印内容。

本地 ThingsVis 制品通过 `TP_THINGSVIS_IMAGE` 加 `TP_THINGSVIS_LOCAL_IMAGE_ID` 选择。执行 `docker image inspect` 核对本机身份后，只跳过该镜像的 registry pull；不同 ID 不会自动拉取或换版本。该选择器的正反例采用替身 Docker 输出，真实 Docker 镜像/配置联验仍由总负责人运行。

下载与 Docker Compose 输出是明确替身；文件系统、Python/Bash 入口、进程信号、子进程锁和 journal 恢复是真实执行。没有调用网络或 Docker daemon，未启动平台、Agent、插件或数据库。此结果不能标为 Linux 正式服务链、新装/数据库升级、Windows 或 J-G4 通过。

## 仍然阻塞的交付

| 项目 | 影响与负责人 | 解除条件 |
| --- | --- | --- |
| 整组签名 release manifest 与发行信任根 | installer/发布负责人；当前 HTTPS 和 updater hash 不能证明全部 payload 的独立发布身份 | 固定可信公钥/签名流程；manifest 覆盖 release commit/version、全部相对路径、摘要和长度；验证错误签名、混发、漏文件、过期/撤回策略，禁止用临时测试 key 冒充正式根 |
| 跨 updater 版本升级 | installer/发布负责人；旧已安装脚本的固定 hash 会安全拒绝新 updater，不能报自动升级闭环 | 使用已审阅新 bootstrap，或完成基于独立 trust root 的 manifest 选择与 updater 变更授权；保持无循环摘要依赖 |
| 原生 Windows | 乙 installer；PowerShell AST、真实系统锁、只读文件/NTFS/ACL、包安装均未本轮实跑 | 原生 Windows 的同套测试与实际新装/升级/恢复；不以 Linux 上 artifact 选择或解析替代 |
| activation 外部副作用协调 | 甲迁移 owner + 乙 installer；journal 故意保留 unknown，不自动重做 DB/服务效果 | 实际固定制品上证明迁移原键核对、重启/中断恢复、兼容/不兼容回退 |
| 四 profile 与完整产品链 | 甲存储 owner + 乙安装/ThingsVis；PG/unknown 守卫保留 | 同一固定制品集完成 J-G1～J-G4；MySQL+TDengine 在 PG 不可达时通过 |
| Agent 执行服务 | 乙 Agent；现有 agent/ 仍是本地持久状态内核 | 接入真实可信任务、进程/容器执行与平台围栏，并通过旧实例/重启/失联组合故障 |

签名方案需由独立公钥验证 manifest；manifest 的签名在 payload 冻结后生成，payload 不内嵌该 manifest 的 hash，从而避免 installer、updater 和管理脚本循环互相摘要。当前仓库提供的 release workflow 只生成部分下载附件的 SHA256SUMS，没有提供可核验的正式签名公钥或相应发布证据；本轮不擅自生成发行身份。
