# T6: HA 插件公开分发准备

## 本次结果

- 分支：`wp/t6-plugin-distribution`，基于安装器 `develop` `c551440`。
- Worktree：`/Users/junhong/Downloads/code/thingspanel-installer/.claude/worktrees/wp-t6-plugin-distribution`。
- 已复核并留存签名任务 run `42436565819` 的双架构 artifacts；没有重新构建、重新签名、推送、合并或发布。
- 本地发布目录：`/Volumes/KIOXIA/CodexTests/ha-public-dist-review-42436565819/`，目录说明和逐文件 SHA-256 在 `PUBLICATION-MANIFEST.md`。
- 真实本机 runtime smoke 记录：`/Volumes/KIOXIA/CodexTests/ha-public-dist-review-42436565819/runtime-smoke/RUNTIME-SMOKE.md`。
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

## 当前阻断

- 公开 Pages 仍为 legacy `main:/`。最新实查：首页和 `install.bash` 为 HTTP 200；catalog、`plugin_bundle.py` 及其依赖 helper 地址仍为 404。当前无 HA 专属正式 Release。
- 默认分支 `main` 当前包含签名入口 dispatcher（提交 `4f81aa2`），其调用的可复用 workflow 来自开放 PR #3 最新提交 `1f2f5a96`。PR #3 仍有 83 个文件；不能整支盲合。
- 最小安装/升级运行链目前只在本地分支 `wp/t6-plugin-distribution`，尚未进入公开 `main`。因此即使发布静态文件或 Release，现有公开安装器仍不能消费它们。
- Pages 仍使用 branch source；workflow 的 `GITHUB_TOKEN` 直接推送不会触发 Pages 构建。使用审核后的普通 main 合并来触发构建，构建后逐个读回公开 URL；不得依赖 14 天 Actions artifact 作为正式分发。
- PR #3 的 Cloudflare Workers Builds 检查失败原因仍未知；在有权限的会话拿到真实日志前，不猜测根因或关闭检查。

## 可审查发布顺序

1. 审查本分支最小代码差异，确认后由负责人决定如何纳入公开 `main`；不需要合并整个 PR #3。
2. 代码接入后，创建 HA Release `plugin-ha-v1.0.0` 并上传本地 `public-release/plugin-ha-v1.0.0/` 四个已验签配套资产。
3. 把本地 `pages/` 文件放入待审 Pages 源 PR。正常审查合并后等待 legacy Pages 构建；如构建需要重试，由有 `Pages: write` 权限的维护者请求构建。
4. 从公网下载 updater、catalog、签名、五个 helper 与 Release 资产，重新核对签名/摘要；在隔离新主机上完成平台超管安装、设备配置/遥测和篡改拒绝检查。

本任务明确禁止推送、合并和发布，因此只准备了本地可审查文件；正式公开变更仍未执行。旧候选 30 分钟验收不能替代上述固定正式版本的公开安装验收。

## 本地验证

- 两个 catalog 使用固定公钥验签成功。
- Agent 和镜像归档的摘要、镜像 ID、架构与 catalog 一致。
- 复用安装器 `stage_plugin_bundle` 以本地文件映射模拟公开 URLs，amd64/arm64 均完成 staging；这是 fixture 校验，不是公网下载或真实平台安装。
- `python3 -m unittest tools.test_plugin_bundle tests.test_plugin_bundle_activation tests.test_reconcile_activation tests.test_update_transaction tests.test_agent_host`：87 项通过。
- Agent Go 测试以 `GOOS=linux GOARCH=arm64 go test -c` 编译后，在隔离 Docker 容器中执行：通过。直接在 macOS 执行 `go test ./...` 不适用，因为 Agent 包仅含 Linux build tags。
- 本机 Docker（linux/arm64）真实冒烟：签名 bundle staging → Agent 启动 → `/v1/catalog` → 接受 generation → `/v1/prepare` → `/v1/grant` → `/v1/activate` → 插件 `/health` HTTP 200 → `/v1/renew` → `/v1/revoke`。测试容器已停止；测试 network 已移除。平台 heartbeat/device-sync 无法通过无网络隔离环境，完整平台验收仍未完成。
- `sh -n install.bash upgrade.sh`、`python3 -m py_compile`（六个 updater/helper）及 `git diff --check` 通过。
- Pages API、PR #3、Release 列表和当前公开 HTTP 状态已核对；没有发布 workflow 运行、Release 创建或 Pages 部署。
