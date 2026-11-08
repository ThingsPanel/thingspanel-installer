# T6: HA 插件公开分发准备

## 本次结果

- 分支：`wp/t6-plugin-distribution`，基于安装器 `develop` `c551440`。
- Worktree：`/Users/junhong/Downloads/code/thingspanel-installer/.claude/worktrees/wp-t6-plugin-distribution`。
- 已复核并留存签名任务 run `42436565819` 的双架构 artifacts；没有重新构建、重新签名、推送、合并或发布。
- 本地发布目录：`/Volumes/KIOXIA/CodexTests/ha-public-dist-review-42436565819/`，目录说明和逐文件 SHA-256 在 `PUBLICATION-MANIFEST.md`。
- 已从本地候选分支移除上一版会重建/重签的 `ha-plugin-publish.yml`；当前不得运行那套流程。

## 固定来源与签名验证

| 项目 | 固定值 |
| --- | --- |
| Actions run | `42436565819`，结论 `success`，入口提交 `4f81aa2a887ebd53dcb27105f720d12135257ec5` |
| 可复用签名 workflow/安装器来源 | `1f2f5a96cf5617b468460634e0616d25e8fecafc` |
| HA 插件 | `1.0.0`，源码 `0b3240e060def50e124495f478a79ea9d1809ab0` |
| SDK | `v1.1.0`，提交 `fa39f24ea7bdcbe781029ec515713e7804250134` |
| Catalog 信任公钥指纹 | `6f6e902e807b86949e9dc3090de82eb932c27c6bd7f95987c8f44de00cca4fe8` |
| amd64 / arm64 artifacts | `13600006489` / `13600385219`；均未过期，原定 2026-11-22 到期 |

GitHub artifact API 的两个 archive SHA-256 与下载后的 ZIP 完全一致。两个 catalog 都使用上述公钥验签通过。每个 catalog 内的 Agent SHA-256、镜像归档 SHA-256 均与本地文件一致；Docker save 归档里的镜像 config ID、操作系统、架构也与 catalog 相符。Agent ELF 架构分别为 linux/amd64 与 linux/arm64。验收未运行 `docker load`，没有操作用户容器或卷。

## 安装器公开路径核对

候选 `tools/plugin_bundle.py` 的固定地址为 `https://install.thingspanel.io` 和 `https://github.com/ThingsPanel/thingspanel-installer/releases/download`。本地使用该候选函数逐架构执行 staging，签名、摘要和下载 URL 检查均通过：

| 内容 | 预期发布位置 |
| --- | --- |
| amd64 / arm64 catalog 与签名 | Pages `plugins/catalog-linux-{amd64,arm64}.json` 及对应 `.sig` |
| Agent | Release tag `plugin-ha-v1.0.0` 下 `tp-agent-linux-{amd64,arm64}` |
| HA 镜像归档 | 同一 Release tag 下 `ha-image-linux-{amd64,arm64}.tar` |
| 固定摘要 helper | Pages 根下 `tools/plugin_bundle.py`、`tools/plugin_catalog.py`、`tools/agent_host.py`、`tools/scope_sync.py`、`config/native/release_manifest.py` |

上述五个 helper 的逐字节 SHA-256 与 workflow 来源 `1f2f5a96` 中 updater 的 `PLUGIN_HELPER_PROFILES` 固定值一致。Catalog 仅存 Release tag 和摘要；下载器按 tag 拼成上表资产 URL。所有正式发布文件已经复制到仓库外的 `public-release/` 与 `pages/` 子目录，供审阅和后续上传。

## 当前阻断

- 公开 Pages 仍为 legacy `main:/`。最新实查：首页和 `install.bash` 为 HTTP 200；catalog、`plugin_bundle.py` 及其依赖 helper 地址仍为 404。当前无 HA 专属正式 Release。
- 默认分支 `main` 当前包含签名入口 dispatcher（提交 `4f81aa2`），其调用的可复用 workflow 来自开放 PR #3 最新提交 `1f2f5a96`。PR #3 仍有 83 个文件；不能整支盲合。
- 当前 `main` 尚无 Agent、插件 bundle 和 updater/helper 安装接线。只发布 Pages 文件或 Release 文件还不能让现有安装器消费它们。必须先审查并纳入 PR #3 的最小安装/升级运行链及其依赖，再发布静态文件与 Release。
- Pages 仍使用 branch source；workflow 的 `GITHUB_TOKEN` 直接推送不会触发 Pages 构建。使用审核后的普通 main 合并来触发构建，构建后逐个读回公开 URL；不得依赖 14 天 Actions artifact 作为正式分发。
- PR #3 的 Cloudflare Workers Builds 检查失败原因仍未知；在有权限的会话拿到真实日志前，不猜测根因或关闭检查。

## 可审查发布顺序

1. 对 PR #3 按安装器运行链做最小差异审查：Agent、bundle/catalog/helper、updater、安装/升级调用接线及必要资源文件；无关 profile、迁移和其它历史改动单独判断。
2. 在允许的代码集成完成后，创建 HA Release `plugin-ha-v1.0.0` 并上传本地 `public-release/plugin-ha-v1.0.0/` 四个已验签配套资产。
3. 把本地 `pages/` 文件放入待审 Pages 源 PR。正常审查合并后等待 legacy Pages 构建；如构建需要重试，由有 `Pages: write` 权限的维护者请求构建。
4. 从公网下载 catalog、签名、五个 helper 与 Release 资产，重新核对签名/摘要；在隔离新主机上完成平台超管安装和篡改拒绝检查。

本任务明确禁止推送、合并和发布，因此只准备了本地可审查文件；正式公开变更仍未执行。旧候选 30 分钟验收不能替代上述固定正式版本的公开安装验收。

## 本地验证

- 两个 catalog 使用固定公钥验签成功。
- Agent 和镜像归档的摘要、镜像 ID、架构与 catalog 一致。
- 复用安装器 `stage_plugin_bundle` 以本地文件映射模拟公开 URLs，amd64/arm64 均完成 staging；这是 fixture 校验，不是公网下载或真实平台安装。
- Pages API、PR #3、Release 列表和当前公开 HTTP 状态已核对；没有发布 workflow 运行、Release 创建或 Pages 部署。
