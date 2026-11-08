# T6: HA 插件公开分发准备

## 本次范围

- 分支：`wp/t6-plugin-distribution`，基于安装器 `develop` `c551440`。
- Worktree：`/Users/junhong/Downloads/code/thingspanel-installer/.claude/worktrees/wp-t6-plugin-distribution`。
- 只准备安装器发布接线；没有改任务一的工作区、密钥或 HA 制品，也没有推送、合并或发布。

## 核实的现有公开契约

`tools/plugin_bundle.py` 的候选实现读取：

| 内容 | 公开位置 | 文件名/格式 |
| --- | --- | --- |
| 每架构签名 catalog | `https://install.thingspanel.io/plugins/` | `catalog-linux-{amd64,arm64}.json` 与各自 `.sig` |
| Agent | GitHub Release `plugin-ha-v<VERSION>` | `tp-agent-linux-{amd64,arm64}` |
| HA 镜像归档 | 同一 GitHub Release | `ha-image-linux-{amd64,arm64}.tar` |
| updater 下载的受信 helper | Pages 网站根目录 | `tools/plugin_bundle.py`、`tools/plugin_catalog.py`、`tools/agent_host.py`、`tools/scope_sync.py`、`config/native/release_manifest.py` |

`config/native/update.py` 把以上 helper 的 SHA-256 固定在安装器代码里；它下载后逐个校验，随后由 helper 验 catalog 签名，并核对 Agent 与镜像归档摘要。目录里的公钥不能替换安装器外置/内嵌信任根。页面只提供入口，真正下载的二进制与镜像须在 Release。

实时核查（2026-11-08）：Pages API 显示 `build_type=legacy`、来源 `main:/`；`/plugins/catalog-linux-amd64.json`、`/plugins/catalog-linux-arm64.json` 和 `/tools/plugin_bundle.py` 均 HTTP 404，`/install.bash` HTTP 200。GitHub Releases 当前有安装器版本，但没有 HA 专属正式 Release。PR #3 (`wp/t6-ha-public-release` → `main`) 有 83 个文件；Linux、macOS、Windows 与 validate 检查通过，Cloudflare Workers Builds 失败，具体原因未知。候选 HA workflow 只上传 14 天 Actions artifact，没有创建 Release 或部署 Pages 文件。

## 本分支准备的最小分发流程

新增 `.github/workflows/ha-plugin-publish.yml`。合入默认分支后，只能从 `main` 手动触发；必须显式将 `publish` 设为 true。它使用同一个已合入的安装器提交构建 Agent、两架构镜像、catalog 与签名，使用现有 `TP_PLUGIN_CATALOG_SIGNING_KEY` Secret，紧接着用仓库的 `config/native/plugin_catalog_trust.pem` 验签。密钥只进入 runner 的权限受限临时文件，不写入 artifact、日志或仓库。签名 Secret 与公钥不配对时会在创建 Release 前失败。

工作流按上表命名 Agent 和镜像，创建 `plugin-ha-v<VERSION>` Release；重复运行只补传缺失资产，遇到已有同名资产摘要不一致即失败，不覆盖。发布前，它也会验证要公开的五个 helper 与 updater 的固定摘要逐项一致。

catalog、签名和 helper 会组成一个待审 PR，目标是 Pages 当前发布源 `main:/`。工作流**不会**尝试用 `GITHUB_TOKEN` 直接推送 main：GitHub 官方说明该 token 产生的提交不会触发 Pages 构建（[Pages 发布源说明](https://docs.github.com/en/pages/getting-started-with-github-pages/configuring-a-publishing-source-for-your-github-pages-site)）。负责人审查并正常合入生成的 PR 后，普通 main 提交触发现有 legacy Pages 构建；之后必须从公开 URL 重新抓取并验证。Release 先于 Pages PR，因此 PR 合入前安装器仍取不到 catalog，不能宣称可安装。

仓库还需允许 GitHub Actions 创建 pull request（`pull-requests: write` 已声明）；若仓库设置禁用该能力，工作流会在 Pages PR 创建步骤失败，Release 可能已完成。重跑会先核对已有 Release 资产摘要，缺失的资产可补齐，冲突资产会失败关闭。

## PR #3 审查边界

禁止整体合入 83 文件 PR。公开分发工作流依赖候选内插件目录构建工具、签名验证器、插件 Agent、updater/helper 激活逻辑与安装/升级入口。应按“安装页触发 → 平台 API/Agent 安装 → updater/helper 下载验签 → Release 取包与 `docker load`”逐段审查并裁切这些功能所需提交；迁移护栏、数据库 profile、ThingsVis、安装器无关的文档/候选改动单独判断，不随分发流程自动纳入。当前 `develop` 还没有候选 workflow 所用的完整 `agent/` 与 `tools/plugin_bundle.py`，所以不能在本分支直接运行正式构建。

## 未完成的发布验收

- 没有可检验的正式签名运行；尚未证明 GitHub Secret 与公钥配对。
- 没有正式 HA Release、Pages catalog/helper，也没有从公网验签、摘要比对或 Docker 加载记录。
- 页面安装与 30 分钟观察是旧候选记录，不能作为本版本正式发布验收。
- PR #3 的 Cloudflare 检查日志仍不可读；发布链与该红灯目前没有可证实的因果关系。
- 最终发布需要将经审查的代码合入默认分支并运行带 `publish=true` 的工作流，再审查/合入自动生成的 Pages PR。任务约束禁止本次推送、合并；用户明确允许公开发布，但这与“不推送、不合并”冲突。本分支只交付可审查结果，不越过该约束。

## 验证

- 对照候选 `tools/plugin_bundle.py`、`config/native/update.py`、`tools/plugin_catalog.py`、`plugins/homeassistant.json` 及 `.github/workflows/ha-plugin-candidate.yml` 核对命名与校验契约。
- Ruby YAML 语法解析和各 `run` 区块的 `bash -n` 检查通过；候选 updater 固定的五个 helper 摘要逐项比对通过。
- 核对 GitHub PR #3、release 列表、Pages API 配置及四个实际公开 URL 的 HTTP 状态。
- 发布 workflow 尚未执行；正式制品/签名/Pages 发布及真实安装均未验证。
