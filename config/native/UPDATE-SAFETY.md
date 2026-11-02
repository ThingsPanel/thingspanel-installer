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
