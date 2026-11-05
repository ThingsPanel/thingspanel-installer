# PH1-S2 — 安装器识别新镜像漏打包迁移

## 判定依据
用**镜像内标记**：同一个后端镜像里是否存在 `/go/src/app/internal/pluginidentity`（插件身份的 Go 源码，与 `sql/33.sql` 同一功能、同一镜像分发，`Dockerfile` 用 `COPY --from=builder /go/src/app .` 整目录带入，`bootstrap` 本来就从同一路径 `docker cp` 取 `sql/`）。

- 有标记 = 新镜像，`sql/33.sql` 必须存在，缺则失败。
- 明确没有标记 = 旧镜像（如已发布的 v1.3.0），跳过并记日志。

为什么不用别的：
- `VERSION_NUMBER`：develop 上仍是 25，而 `33.sql` 已存在（它本来就不走 sys_version），无法区分。
- 镜像 tag/版本号：本地 A1 镜像用任意 tag 和 image ID 固定，无法解析。
- 发行清单声明迁移列表：要改后端发布流程和清单格式，改动大。

## 改动
`config/native/prepare.py`：新增 `image_has_marker()`、`required_extra_sql()`；`bootstrap()` 调用它们替换原来的“有就用、没有就悄悄跳过”。报错写明缺的文件（`sql/33.sql`）和镜像名。

## 无法判定时的默认行为
`docker cp` 返回的不是“找不到文件”（例如 Docker 异常）→ **失败**，不猜。理由：中止可以重试，静默跳过会造成无报错的插件故障。

## 测试
新增 `tests/test_migration_guard.py`（7 个）：新镜像有文件、新镜像缺文件（失败，信息含文件名和镜像名）、旧镜像无文件（跳过+日志）、旧镜像有文件（照常应用）、标记存在/明确不存在/无法判定（失败）。
另改了 `tests/test_p1_auto_steps.py` 里 bootstrap 的端到端 mock 测试：新增对 `image_has_marker` 的 mock（有 33.sql 视为新镜像）。

全量结果（证据：`/Volumes/KIOXIA/archive/evidence/2026-11-05-PH1-S2/tests.log`）：
- `tools/test_plugin_install.py`：20 个，全部通过。
- `python3 -m unittest discover -s tests`：120 个，**14 个错误，全部在 `tests/test_download_failure.py`**（install/upgrade 的 shell 脚本在本机超过测试内置的 20 秒超时）。**这些错误不是本改动引起**：在未改动的 develop `4a8f286` 干净检出上单独跑同一用例（`test_upgrade_each_download_failure_preserves_old_files`）同样失败（超时）。除这 14 个外，其余 106 个通过，含新增 7 个。
- 因此任务要求的“全部通过”**未达成**：`test_download_failure` 在本机是已存在的环境性失败，未在这里修。

## 没做
- 未用真实 Docker/镜像做端到端验证（单元测试用 mock，不能当端到端验收）。`docker cp` 对不存在路径的报错措辞（“could not find the file”）按 Docker 常见行为匹配，需在真实镜像上确认一次。
- 若后端以后重命名/移除 `internal/pluginidentity`，需同步修改 `EXTRA_SQL_MARKER`。
- 未推送、未合并。
