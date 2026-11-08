# T6: HA 插件公开分发准备

## 本次结果

- 分支：`wp/t6-plugin-distribution`，基于安装器 `develop` `c551440`。
- Worktree：`/Users/junhong/Downloads/code/thingspanel-installer/.claude/worktrees/wp-t6-plugin-distribution`。
- 已复核并留存签名任务 run `42436565819` 的双架构 artifacts；没有重新构建或重新签名。
- 因远端原先没有 `develop` 分支，将本地 develop 基线和最小 T6 运行代码推送到新远端 `develop`。分支头为 `db89dc1c4f7385b9d356f4227d8564e586f74554`，未合入 `main`。
- 为不公开整个开发分支，创建 Pages 静态源分支 `codex/ha-plugin-public-site-20261108`，基于原公开 `main` 文件树，只叠加安装入口、compose/native updater 和固定摘要 helpers/catalog。Pages 源切到该分支；build `72946f1edc51f29ee93462458fd52ea6991492db` 状态 `built`。
- HA 插件 Release `plugin-ha-v1.0.0` 已公开，四个 Agent/image 资产摘要与签名 catalog 相符；没有创建 All-in-One 安装器版本。
- 本地发布目录：`/Volumes/KIOXIA/CodexTests/ha-public-dist-review-42436565819/`，目录说明和逐文件 SHA-256 在 `PUBLICATION-MANIFEST.md`。
- 真实本机 runtime smoke 记录：`/Volumes/KIOXIA/CodexTests/ha-public-dist-review-42436565819/runtime-smoke/RUNTIME-SMOKE.md`。
- 公网分发和 updater 路径的运行证据：`/Volumes/KIOXIA/CodexTests/ha-public-dist-review-42436565819/public-pages-smoke/PUBLIC-PAGES-SMOKE.md`。
- 已从本地候选分支移除上一版会重建/重签的 `ha-plugin-publish.yml`；当前不得运行那套流程。
- 从 PR #3 抽取了安装运行所需的最小代码集到本分支：Agent renewal、签名 catalog/bundle 下载和验证、updater 安装/升级接线、compose Agent socket 挂载及 HA 插件清单。没有复制 PR 的发布 workflow、无关 prepare.py 变更或其它 profile 改动。
- 本地资源包现已补入 `pages/config/native/update.py`。该文件 SHA-256 为 `f371cabfab4d2f5fbb37cc892e5b7e0cfbc9436b7a615d91da67ec341b1291aa`，与四个安装/升级入口固定值一致。

## 固定来源与签名验证

| 项目 | 固定值 |
| --- | --- |
| Actions run | `42436565819`，结论 `success`，入口提交 `4f81aa2a887ebd53dcb27105f720d12135257ec5` |
| 可复用签名 workflow/安装器来源 | `1f2f5a96cf5617b468460634e0616d25e8fecafc` |
| HA 插件 | `1.0.0`，源码 `0b3240e060def50e124495f478a79ea9d1809ab0` |
| SDK | `v1.1.0`，提交 `fa39f24ea7bdcbe781029ec515713e7804250134` |
| Catalog 信任公钥指纹 | `6f6e902e807b86949e9dc3090de82eb932c27c6bd7f95987c8f44de00cca4fe8` |
| amd64 / arm64 artifacts | `13600006489` / `13600385219`；均未过期，原定 2026-11-22 到期 |

GitHub artifact API 的两个 archive SHA-256 与下载后的 ZIP 完全一致。两个 catalog 都使用上述公钥验签通过。每个 catalog 内的 Agent SHA-256、镜像归档 SHA-256 均与本地文件一致；Docker save 归档里的镜像 config ID、操作系统、架构也与 catalog 相符。Agent ELF 架构分别为 linux/amd64 与 linux/arm64。随后将 arm64 插件镜像加载到本机 Docker，用一次性隔离网络完成真实 Agent 安装和健康检查；测试容器、Agent 和网络已清理，加载后的镜像及外部证据目录保留。没有触碰既有容器或卷。

## 安装器公开路径核对

候选 `tools/plugin_bundle.py` 的固定地址为 `https://install.thingspanel.io` 和 `https://github.com/ThingsPanel/thingspanel-installer/releases/download`。本地使用该候选函数逐架构执行 staging，签名、摘要和下载 URL 检查均通过：

| 内容 | 预期发布位置 |
| --- | --- |
| amd64 / arm64 catalog 与签名 | Pages `plugins/catalog-linux-{amd64,arm64}.json` 及对应 `.sig` |
| Agent | Release tag `plugin-ha-v1.0.0` 下 `tp-agent-linux-{amd64,arm64}` |
| HA 镜像归档 | 同一 Release tag 下 `ha-image-linux-{amd64,arm64}.tar` |
| 固定摘要 helper | Pages 根下 `tools/plugin_bundle.py`、`tools/plugin_catalog.py`、`tools/agent_host.py`、`tools/scope_sync.py`、`config/native/release_manifest.py` |
| 固定摘要 updater | Pages `config/native/update.py`；bash/PowerShell 安装和升级入口均校验上述 SHA-256 |

五个 helper 的逐字节 SHA-256 与 workflow 来源 `1f2f5a96` 中 updater 的 `PLUGIN_HELPER_PROFILES` 固定值一致；updater 的 SHA-256 与四个入口脚本一致。Catalog 仅存 Release tag 和摘要；下载器按 tag 拼成上表资产 URL。所有正式发布文件已经复制到仓库外的 `public-release/` 与 `pages/` 子目录，供审阅和后续上传。

## 当前公开状态与验收边界

- 刚复核的公开入口 `install.sh`、`install.bash`、compose、updater、catalog、签名和固定摘要 helper 均返回 HTTP 200；GitHub Release 的四个资产都公开，API SHA-256 与本地 manifest 一致。
- 使用从公网下载的 updater 实际执行 `prepare_optional_plugin` 下载和校验路径：五个 helper 的摘要全部命中 updater 固定值，amd64/arm64 catalog 验签和 Agent/image digest 校验都通过。
- 使用 updater 生成的 arm64 bundle，在无外网、无平台 backend 的隔离 Docker 网络里完成 Agent catalog、prepare、grant、activate、HA `/health` 200、renew、revoke。测试容器与网络已清理；没有配置 HA 接入点、没有测试设备上下行。
- 未完成平台超管在真实资源中心签发身份、后端 heartbeat/device config、设备遥测/命令等完整集成验收；该范围本次明确不做。
- PR #3 仍开放，Workers Builds 检查仍失败且根因未知；它不阻断本次 Pages 构建和公开下载链路。本次没有合并整个 PR #3，也没有改 `main`。

## 后续维护

1. Pages 当前由 `codex/ha-plugin-public-site-20261108:/` 发布；后续更改应先更新此静态源分支并请求 Pages build，不能假设 `main` 自动同步。
2. 将来如决定合入 `main`，先审查公开分支与 `develop` 的差异，并把 Pages source 恢复到 `main:/` 或建立明确的同步流程。
3. 如扩展到平台验收，使用新隔离栈完成 SYS_ADMIN 资源中心安装和受管身份验证；明确排除 HA 站点配置、设备上报及命令下行，直到另行授权。

## 本地验证

- 两个 catalog 使用固定公钥验签成功。
- Agent 和镜像归档的摘要、镜像 ID、架构与 catalog 一致。
- 复用 `stage_plugin_bundle` 直接从实际公开 URL 下载并校验，amd64/arm64 均 staging 成功；public updater 的 `prepare_optional_plugin` 也下载五个 helper 并通过固定摘要校验。
- `python3 -m unittest tools.test_plugin_bundle tests.test_plugin_bundle_activation tests.test_reconcile_activation tests.test_update_transaction tests.test_agent_host`：87 项通过。
- Agent Go 测试以 `GOOS=linux GOARCH=arm64 go test -c` 编译后，在隔离 Docker 容器中执行：通过。直接在 macOS 执行 `go test ./...` 不适用，因为 Agent 包仅含 Linux build tags。
- 本机 Docker（linux/arm64）真实公网冒烟：public updater 下载并固定 helper → 公开 Release 下载 signed Agent/image → Agent `/v1/catalog` → 接受 generation → `/v1/prepare` → `/v1/grant` → `/v1/activate` → 插件 `/health` HTTP 200 → `/v1/renew` → `/v1/revoke`。测试容器已停止，网络已移除。平台 heartbeat/device-sync 因未连接平台 backend 而未验收。
- `sh -n install.bash upgrade.sh`、`python3 -m py_compile`（六个 updater/helper）及 `git diff --check` 通过。
- Pages build `72946f1` 为 `built`；公开静态文件与四个 Release 资产均已读回。正式安装器版本没有发布。
