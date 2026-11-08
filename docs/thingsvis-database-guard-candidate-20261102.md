# ThingsVis 数据源检查候选

当前正式 Compose 固定 PostgreSQL URL，尚无 MySQL/TDengine profile 选择或四组合支持。`config/native/prepare.py` 对实际 Compose 的 ThingsVis `DATABASE_URL` 做 scheme 检查，仅允许 `postgres` / `postgresql`；不会输出 URL 或凭据。新安装先用不插值的 Compose 结果检查，在生成缺省凭据后再检查插值结果；不连接数据库、不启动服务。测试用 Docker Compose 渲染和临时替身，不使用已有部署。

**覆盖边界：**候选安装和升级入口在覆盖已有 Compose 前，尝试读取有效的 ThingsVis URL；不能确认其使用 PostgreSQL 时停止，旧文件不覆盖。Linux/macOS 的两个入口已用命令替身和临时目录测试；Windows PowerShell 入口已加入对应检查，但本机无 PowerShell，尚未执行。既有 Compose 缺 ThingsVis 服务或 URL 时也会停止，不能将其视为 MySQL：这是保护未知旧配置的保守行为，可能阻止旧版或自定义安装升级，仍须核对已支持的历史版本和迁移方案。先读取插值后的 Compose，失败时用不插值结果；无法判断的变量化 scheme 仍拒绝。直接运行 `docker compose` 绕过入口检查。

检查只确认 URL scheme，不证明 PostgreSQL 的数据库/schema、迁移、健康、应用功能或任一完整存储组合。Windows、历史版本、实际完整安装/升级和最低支持 Docker Compose 版本均未实测；不能宣称 installer 全面拒绝所有不支持的组合，更不能据此宣称 MySQL 支持。

验证命令：`PYTHONDONTWRITEBYTECODE=1 python3 -m unittest -v config/native/test_thingsvis_profile.py`。测试不启动容器，入口测试使用命令替身；真实完整安装、升级、Windows 和真实数据库均未验证。
