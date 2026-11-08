# C01 配置安全更新：本轮实施与验证记录

基线是远端 installer worktree `05470e09616feaee8979a5b0ad6206028986da2a` 的完整文件镜像。仅在本地镜像修改；未部署、未启动真实栈、未连接数据库、未提交或推送。本文属于 2026-11-02 收尾批次。

## 已实施

- 四个 Bash/PowerShell 正式入口使用同一固定摘要 Python updater。完整暂存 9 个下载文件，校验后连同受信任 updater 提交；下载失败不改变原配置内容/模式。
- 同一内核锁覆盖恢复、暂存、替换、native 激活；journal、备份和替换使用刷新/原子写入。替换中断、原文件缺失、恢复再次中断均有对应行为。
- 原 `.env`/token 与原文件快照在每个替换前复核；第三方修改、损坏 journal/备份、symlink、不明来源目录失败关闭。恢复只恢复本事务的已知旧/新内容，不覆盖人工改动。
- PG/unknown 守卫保留。暂存 Compose 锚定原 project directory 和 `.env`，native 子进程也不继承外部 `COMPOSE_FILE`。不会执行新下载的 prepare 来证明自身安全。
- `activation_started` 之后失败保留现场并要求协调外部效果；不会把配置恢复叫作数据库回滚。Windows 原 `compose down --remove-orphans` 已删除。
- pkg/Inno 不再直接覆盖 Compose 和 upgrade/uninstall；macOS postinstall 不再 chmod 既有管理脚本。PR 和 release 共用三平台事务测试，发布构建依赖通过结果。
- 修复一处旧 fixture：用于证明 PG 可通过的样本补齐已有 backend 守卫要求的 PG 字段，没有放宽生产守卫。

## 实际执行结果

本地平台为 Linux，Python 3.12；没有 Docker CLI、PowerShell、Windows/macOS 原生打包器。

| 检查 | 结果 | 证据范围 |
| --- | --- | --- |
| `PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s tests -v` | exit 0，28 tests | 包含原 14 个下载失败子边界，4 个 bootstrap 子边界，20 个替换前后异常子边界，6 个跨进程 SIGKILL 阶段及恢复再中断；各子边界不重复计入 28 方法数 |
| 旧 prepare 日志脱敏与 ThingsVis PG/unknown 的无 Docker 子集 | exit 0，6 tests | 原 guard、拒绝 MySQL/不明配置、入口在下载前拒绝不支持配置；不含真实 Compose 解析 |
| Bash 语法：install.sh/install.bash/upgrade.sh/macOS build.sh/postinstall | exit 0 | 仅语法，不执行打包/安装 |
| `git diff --check` | exit 0 | 无空白格式错误 |
| updater 与 3 个 Python 测试文件按 Python 3.8 grammar 解析 | exit 0 | 证明语法最低版本；本轮实际解释器为 3.12 |

日志在收尾工作区：`implementation/installer-transaction-tests.log`、`implementation/installer-guard-subset-tests.log`。初次尝试整个旧 `config/native` 测试目录的日志 `implementation/installer-existing-guards.log` 保留失败事实：真实配置解析缺少 Docker CLI，旧 PG fixture 不完整；fixture 修正后重跑上述 6 项成功。本轮没有把真实解析改为模拟来声称其通过。

## 仍须在对应平台验证

- 三平台 CI 尚未运行；Windows 的实际文件锁/写穿替换、PowerShell AST，macOS 文件系统行为与 Bash 入口等待 CI 证据。Linux 上的 Windows artifact 选择用例只能证明负载选择，不能充当 Windows 系统验收。
- 本轮没有编译或执行 pkg/Inno。新增包装测试验证其清单不直接携带配置覆盖负载；实际包安装仍需隔离平台环境。
- 未执行真实 Docker Compose、native migration 或服务启动；这些属于下一阶段集成门禁。未解除 PG/unknown 守卫，也未承诺多 DB 安装已通过。
- 操作进入 activation 后的服务/DB 协调恢复、网络共享文件系统、断电耐久性及 Windows ACL 的最终行为不能由 Linux 进程故障注入证明。遇到未知 activation 必须保留现场，不能删除 journal 自动重试。
- updater 固定摘要与负载 HTTPS 来源并不等同于完整发布包签名。现有一行 bootstrap 的分发信任继续沿用原机制。

恢复协议与命令见 `config/native/UPDATE-SAFETY.md`；测试运行方式见 `tests/README.md`。
