# Installer 后端关系库防护候选（未发布）

现有 Compose 与固定 ThingsVis v1.0.23 都使用 PG，installer 的既有 ThingsVis URL 检查单独无法识别“平台后端 MySQL、ThingsVis 仍是 PG”。本候选在 native `prepare.py` 和 Linux/Windows 安装/升级旧配置检查中加入后端字段核验：要求已知 `GOTP_DB_PSQL_HOST/PORT/USERNAME/DBNAME`，本地 host 为 `postgres` 时须有对应服务；拒绝已知 MySQL/MariaDB 字段、非 PG 的已知 provider/driver/type 和非 PG 的后端 DATABASE_URL。PG 外部 host 不因不存在本地 postgres 服务而被单独拒绝。异常文案不包含数据库 URL。

Linux `install.bash`、`upgrade.sh` 的旧配置检查在网络下载或覆盖前运行；`prepare.py` 在写入配置或拉镜像前运行。Windows 的 `install.core.ps1`、`upgrade.ps1` 同样加入候选检查，但本机没有 PowerShell，尚未运行验证。已有的其它未提交修改保留；本候选还未提交或发布。

`PYTHONDONTWRITEBYTECODE=1 python3 -m unittest -v config.native.test_backend_database_guard_candidate`：9 项通过，涵盖实际渲染的当前 PG Compose、外部 PG、显式 MySQL 与非 PG selector、缺服务、重复变量、ThingsVis MySQL 以及 Linux 安装/升级在旧配置检测失败时不下载、不覆盖。`bash -n install.bash`、`bash -n upgrade.sh`、`git diff --check` 通过。未执行此前系统中止的历史测试；本次也没有重新证明历史标签、Windows 或真实完整安装/升级兼容。

**本候选不是完整四 profile 认证或无隐藏 PG 保证。** 检查只识别当前已知后端环境字段，不验证实际服务器身份、other services/Encore、所有未来 selector 或 datasource 配置；保留 PG 字段同时以未识别方式启用 MySQL 仍可能绕过。缺项、未知 selector 值或不可解析旧配置可能被拒绝，需真实旧部署兼容验证；`--no-interpolate` fallback 也不等于实测所有历史环境。PowerShell 分支仅静态审阅，须在 Windows PowerShell 与 PowerShell 7 验证。现有通用 `command()` 可能将 Compose stderr 原文中的 ERROR/FATAL 行纳入异常，敏感插值泄漏风险须另查。

经用户授权前不得正式启用 MySQL：甲的连接/迁移合同、ThingsVis 字段和登录语义、真正独立 MySQL 客户端/账本及安全预检仍未定；MySQL/TDengine 必须证明没有 PG 可达，MySQL/Timescale 只允许因时序引入已声明 PG。明确配置选择、四 profile 唯一正式 installer 接线、数据搬迁/回退、旧 writer 退出和联合故障测试均未完成。
