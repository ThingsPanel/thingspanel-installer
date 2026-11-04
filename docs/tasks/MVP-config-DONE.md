# MVP-config 完成记录

日期：2026-11-05

仓库：`thingspanel-installer`

worktree：`/Users/junhong/Downloads/code/thingspanel-installer-develop-review/.claude/worktrees/wp-mvp-config`

分支：`wp/mvp-config`，基于 installer develop `a540634`

## 修改

- 默认 backend compose 同时提供 A1 的 `GOTP_RELATIONAL_*` PostgreSQL 配置和旧版 `GOTP_DB_PSQL_*` 配置；Redis、MQTT 和 PostgreSQL 数据库/卷设置未改。
- `prepare.py` 与 `update.py` 在检测到 relational 配置时校验 driver、平台数据库地址、端口、库名、用户和密码字段；缺项、未插值变量或非 PostgreSQL driver 会失败关闭。仅旧键的已安装 compose 仍可通过现有升级预检。
- `update.py` 对 legacy-only 历史/自定义 compose 继续兼容；出现 `GOTP_RELATIONAL_*` 时严格校验新配置，镜像能力不从 tag 推断。
- 同步更新四个受 updater 摘要保护的入口脚本固定值。未改数据库迁移或用户数据路径。

## 镜像与升级兼容

- 安装器默认镜像仍是 `thingspanel-go:v1.3.0`。后端仓库的 `v1.3.0` 源码 `initialize/pg_init.go` 使用 `db.psql.*` 配置，因此 compose 保留旧键供已发布镜像和既有部署继续使用；新增 relational 键供 A1 构建读取。A1 新镜像尚未发布，本任务没有声称 compose 默认镜像已包含 A1 代码。
- 后端 `v1.3.0` 源码检查只能证明该源码版本读取旧配置键，未在本地拉取/运行已发布镜像验证其摘要和运行行为。实际第一阶段验证 A1 时需用明确标记的本地 A1 镜像覆盖 `TP_BACKEND_IMAGE`；旧安装升级预检接受仅旧键的 compose，新默认 compose 同时满足旧、新两套配置契约。
- 成功升级会将目标版本的 `docker-compose.yml` 作为受管制品暂存并验证；它在 `COMMON` 文件集内，验证通过后由事务替换。激活路径再执行 bootstrap、`docker compose up -d --wait` 和 verify。若阶段验证失败，旧配置不变；若中途失败，事务依阶段记录恢复或保留 activation journal，不宣称数据库回滚。目标版本可以是历史或自定义 legacy-only artifact，因此 updater 不对所有暂存 compose 强制新增键。
- 直接把 A1 镜像放进 legacy-only 旧 compose 时，现有配置预检会兼容这份历史配置，无法从镜像 tag 可靠识别 A1 能力，可能放行而缺少 relational env。没有可信镜像能力标记时不猜版本；MVP-RUN 必须使用新默认 compose 或显式补齐 relational 环境变量，并实测启动。新默认 compose 已有 A1 所需变量。
- `runtime.encore.json`/`runtime.notification.json` 仍指向 compose PostgreSQL 的独立 Encore 数据库；本任务未改变它们，也未给 Encore 服务额外加 driver，因为默认 Encore 资源本身由 runtime 配置连接 PG。

## 验证

- `python3 config/native/test_backend_database_guard_candidate.py`：16 项通过。覆盖默认 compose 的 A1 字段、缺字段拒绝、旧键-only升级兼容、非 PG 拒绝及凭据不进入错误文本。
- `python3 tests/test_update_transaction.py ValidatorTests.test_pg_unknown_and_mysql_guards_remain_closed ValidatorTests.test_a1_relational_pg_profile_is_required_when_declared ValidatorTests.test_legacy_only_compose_remains_upgrade_compatible ValidatorTests.test_staged_legacy_only_compose_remains_supported ValidatorTests.test_entrypoint_helper_pins_match_reviewed_bytes`：5 项通过。
- `git diff --check`：通过。
- 未启动容器、数据库或真实栈；未验证 A1 镜像运行及迁移结果。

## 提交

- 当前任务交付提交位于分支 HEAD，标题为 `fix(installer): align default postgres config with A1`。
- 当前分支提交列表以 `git log --oneline develop..HEAD` 为准。
