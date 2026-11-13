# R9 遗留事项：给各负责会话的任务说明

安装器仓库里能做的 R9 工作已做完（见 `R9-DONE.md`）。下面几项不在安装器仓库，或需要发布负责人的动作，各自可以直接复制给对应会话。
所有任务共同约束：不得把凭据写进仓库或文档；验收证据放仓库之外。

## 1. 后端：运行时版本号覆盖（解决"beta 晋升正式版不重新构建"）

- 现状：后端版本号来自 `pkg/global/global.go` 的 `VERSION_NUMBER` 与镜像里的版本字符串，编译进二进制。平台"检查更新"和 Agent 用它判断当前版本。beta 通过后晋升正式版，目前只能重新编译，二进制就不再是测过的那一个。
- 要做：让 `ThingsPanel-Go --version`（以及平台对外报告版本的地方）优先读环境变量 `TP_VERSION_OVERRIDE`；未设置时行为不变。值必须匹配 `^v?\d+\.\d+\.\d+(-beta\.\d+)?$`，否则忽略并记日志。
- 安装器侧配合：Compose 里给 backend 注入该变量。安装器仓库由 R9 会话在后端合入后补一行，后端卡不必碰安装器。
- 不要动：数据库版本（`sys_version`）判断逻辑。数据库升级仍由迁移决定。
- 完成标准：单元测试覆盖设置/未设置/非法值；beta 镜像在设置 `TP_VERSION_OVERRIDE=v1.4.0` 后，`--version` 与系统版本接口都返回 v1.4.0。
- 参考：`encore-api/system/api.go:26` 的 `versionPattern` 已接受 `-beta.N`，不用改。

## 2. 后端/前端：升级辅助镜像不可用时的界面提示

- 现状：Agent 预检时若辅助镜像（默认 `docker:27-cli`）拉不到，返回错误码 `upgrade_helper_image_unavailable`，站点不改动，日志里有 `UPGRADE-HELPER-IMAGE:` 行和 `docker save/load` 离线步骤。界面目前没有对应文案，会显示原始错误码。
- 要做：后端把该错误码映射成可读消息，前端显示："升级需要的辅助镜像拉取失败，当前站点未做任何改动。请在服务器上执行 `docker pull docker:27-cli`，离线环境按安装目录日志中的 `UPGRADE-HELPER-IMAGE` 提示用 `docker save/load` 导入。"
- 完成标准：触发该错误时界面显示上面的文案，不是错误码。

## 3. 后端仓库：合并 `wp/R8-ci-services`

- 原因：R9 的真实验收用的 Encore 镜像是对后端 `origin/develop` 套用 `scripts/build-encore-image.sh` 副本构建的。该分支合并后，镜像构建走正式脚本，R9 的临时副本可以不用。
- 对应会话：[r8-ci-services](#local_fb76f324-4fcb-4399-8aa5-775204bccbaf)。合并是否执行由负责人决定，R9 不替他合。

## 4. 发布负责人（B2）：发布侧动作

这些只有持有签名密钥的人能做，R9 不能代做，也不会生成或保存任何私钥。

1. 生成 beta 专用的发布签名密钥，私钥离线保管；公钥 PEM 交给安装器的 `OFFICIAL_RELEASE_TRUST_PEM`（或测试期用 `TP_RELEASE_TRUST_ROOT` 指定），按 `UPDATE-SAFETY.md` 的流程重新钉哈希。
2. 选定 beta 发布源 HTTPS 地址（Pages 或对象存储），放 `release-manifest.json(.sig)`；用 `tools/sign_release.py` 签名。
3. 发布带 `system` 角色的 tp-agent 二进制，以及签名的 `plugins/plugin-catalog.json(.sig)` 和 `homeassistant-image.tar`；`agent_sha256` 与镜像归档摘要必须和 catalog 一致。
4. 新装机器如要从 beta 源取 bundle，安装命令前设置 `TP_BUNDLE_PAGES_BASE`、`TP_BUNDLE_RELEASE_BASE`（用法见 `R9-BETA-RUNBOOK.md`）。
5. 以上完成后，按 `R9-BETA-RUNBOOK.md` 第 6 节"提升为正式版"走 beta → 正式版（第 7 节处理信任根）。

## 5. 仍待验证（需要真实环境，R9 会话可在下一轮做）

| 项 | 现状 | 怎么验 |
| --- | --- | --- |
| `catalog-sync` | 6 个单元测试，用真实 openssl 签名，docker/下载是模拟的 | 真实 Docker + HTTPS 源上跑一次，含缺镜像 `docker load` |
| `TP_BUNDLE_*` 新装机器路径 | 21 个测试里有覆盖，未在真实新机器上跑 | 空机器一行命令装机，bundle 来自 beta 源 |
| services.json 竞争重试 | 只有单元测试 | 真实栈上创建设备后立刻 `upgrade.sh`，应自动重试通过 |
| beta 备份恢复到正式版机器 | 真实验收里 `restore_failed_rolled_back`，站点无改动；怀疑是我的验证镜像缺 `27.sql`，未证实；日志只剩一行，栈已清理 | 用完整后端镜像重做一次；若仍失败，读 Agent 恢复日志定位 |
| 界面点击 | 验收只用了 `tp-agentctl` | 在界面上点"检查更新/升级"，确认 beta 显示与"已是最新"判断 |
| 回滚耗时 | 坏 beta 自动回滚用了 339 秒 | 确认是镜像拉取/健康检查等待所致，评估是否要缩短超时 |
