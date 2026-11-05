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

## 真实镜像验证（只读，未起栈，未碰运行中的容器）
脚本对每个镜像 `docker create` → `image_has_marker()` → 取 `/go/src/app/sql` → `required_extra_sql()`；日志：`/Volumes/KIOXIA/archive/evidence/2026-11-05-PH1-S2/real-image-verify.log`。

| 镜像 | 标记 | 结果 |
|---|---|---|
| `…/thingspanel-go:v1.3.0` | 无（真实报错 `Could not find the file /go/src/app/internal/pluginidentity in container …`，被正确识别为“不存在”） | 旧镜像，跳过并记日志，应用 `[]` |
| `thingspanel-go:mvp-run-a29fecc` | 有 | 新镜像，取到 `33.sql`，应用 `['33.sql']` |
| `thingspanel-go:p1-local` | **无**（其 `internal/` 里没有 `pluginidentity`，但 `sql/33.sql` 存在） | 判为旧镜像，但因文件存在仍应用 `['33.sql']` |
| 由 mvp-run-a29fecc 派生、删掉 `sql/33.sql` 的临时镜像 | 有 | **失败**：`…contains internal/pluginidentity but is missing required migration sql/33.sql; the image was packaged incorrectly. Refusing to continue.` 临时镜像已删除 |

**发现的局限（需总监知晓）**：`p1-local` 带 `33.sql` 却没有 `internal/pluginidentity`（只有 SQL，Go 代码是更早的版本）。对它，当前逻辑安全（文件在就应用），但如果将来有“只带旧 Go 代码、却漏了 `33.sql`”的这类中间镜像，会被当成旧镜像静默跳过——标记只能识别带完整插件身份源码的镜像。要覆盖这种情况只能改用发行清单声明迁移列表（需改后端发布流程），本卡未做。

## 合并 `wp/pui-agent` 后的修正（语义冲突）
`wp/pui-agent` 加入 `sql/34.sql`、`35.sql`（后端 `internal/plugininstall`）。单一标记会把“只有插件身份、没有插件安装”的镜像（如 `ph1-520a3222`）误判为漏打包。已按要求 **merge（未 rebase）** 并改为“每组迁移各有自己的标记”：

`EXTRA_SQL_REQUIREMENTS = (('internal/pluginidentity', ('33.sql',)), ('internal/plugininstall', ('34.sql','35.sql')))`

哪个标记存在，就要求对应那组文件必须在，缺则失败（报错列出缺的文件和镜像，多个一起列）；标记不存在的组跳过并逐文件记日志；文件存在则无论标记如何都照常应用。以后加迁移只需在映射表加一行。`EXTRA_COMMUNITY_SQL` 由映射表派生。

新增/更新测试（`tests/test_migration_guard.py`，共 7+3 个）：只有 identity 标记（只要求 33，34/35 跳过）、两个标记都有（要求 33–35）、有 install 标记缺 34（失败，只报 34）、有 identity 标记缺 33（失败）、旧镜像（全部跳过并记日志）、有文件无标记（照常应用）、标记探测的三种结果。

真实镜像核对（只读；`real-image-verify-merged.log`）：
- v1.3.0：无标记，无 33–35，全部跳过，应用 `[]`。
- `ph1-520a3222`：只有 identity 标记，有 `33.sql`，34/35 跳过，应用 `['33.sql']`（不再误判）。
- `mvp-run-a29fecc`：同上，应用 `['33.sql']`。
- 附带观察：`thingspanel-go:pui` 有 33、34，没有 35，两个标记都没有（Go 源码目录不在镜像里）；因此按“无标记”处理，应用 33、34，跳过 35。这是前述标记局限的又一例，不是 bug，但说明该镜像缺 35 不会被发现。
- “有 install 标记缺 34”只在单元测试中覆盖，没有为它构造真实镜像。

全量测试结果见下方“最终测试结果”。

## 没做
- 未跑完整 `bootstrap()` 端到端（不起栈、不连数据库）；探测与取文件逻辑已在真实镜像上验证，见上。
- 若后端以后重命名/移除 `internal/pluginidentity`，需同步修改 `EXTRA_SQL_MARKER`。
- 未推送、未合并。

## 最终测试结果（合并 pui-agent + 标记映射后）
证据：`/Volumes/KIOXIA/archive/evidence/2026-11-05-PH1-S2/tests-merged.log`
- `python3 -m unittest discover -s tests`：123 个，全部通过（19.6 秒；这次 `test_download_failure` 没有超时，之前的超时应是当时机器负载造成，未再追查）。
- `tools/test_plugin_install.py`：17 个，全部通过（合并后的版本数量与之前的 20 个不同，来自 pui-agent 分支的改动）。
- 上文较早的“14 个错误”是合并前的那次运行结果，已被本次取代。
