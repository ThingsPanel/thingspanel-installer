# 安装器配置更新的隔离回归

本目录直接测试正式入口和 `config/native/update.py`。所有目标目录均为临时目录，下载和 Compose 输出采用明确的替身；测试不访问网络，不连接 Docker daemon，不启动服务，不操作数据库。

```sh
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s tests -v
bash -n install.sh install.bash upgrade.sh
```

`test_download_failure.py` 保留原 14 个下载失败边界：`install.bash`、`upgrade.sh` 各 7 次失败，逐个检查原配置内容、文件模式、`.env` 和 token，确认未启动服务。另加两入口的 updater 下载失败及固定摘要不符，共 4 个 bootstrap 边界。`test_update_transaction.py` 补充后两个管理脚本下载失败、完整暂存、校验失败、版本不符、摘要混发、20 个替换前后异常点、缺失原文件恢复、人工修改冲突、损坏 journal/备份、同进程与跨进程锁、activation 失败封禁，以及原项目目录/`.env` 和日志脱敏。

POSIX 子进程测试在 6 个实际阶段发送 `SIGKILL`，由第二进程读取磁盘 journal 恢复；另一次在恢复途中发送 `SIGKILL` 后重试。故障钩子只通过测试构造函数注入，正式 CLI 没有注入参数。测试并未把模拟的 Compose 输出当成实际容器验收。

GitHub `update-transaction` job 在 Linux、macOS、Windows 执行此目录。Windows 跳过 Bash 入口、POSIX `SIGKILL` 和需要特殊权限的 symlink 用例，运行真实 Windows 文件替换/锁路径和其余事务测试，并解析 PowerShell 入口。首次 CI 成功前，不能将这些平台列为已实测通过。

旧 `config/native/test_*.py` 中还有需要真实 Docker Compose **CLI 配置解析**和历史 Git tags 的用例。云端缺少 CLI、镜像 checkout 缺少 tags 时，应如实记录未执行/失败原因，不替换其真实解析为 fixture 后宣称通过。
