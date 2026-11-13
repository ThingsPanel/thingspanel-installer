# beta 发布与测试操作清单（R9）

给接手发布和测试的同事。目标：先发 `vX.Y.Z-beta.N`，测试通过后把**同一批构建**提升为正式版 `vX.Y.Z`，不重新构建。
命令里的 `<…>` 是你要填的值；本文不含任何真实发布源地址，因为发布源由你们决定（见 §0）。

## 0. 先搞清楚三样东西

| 名字 | 是什么 | 谁用 |
| --- | --- | --- |
| **发布源** `<SOURCE>` | 一个 `https://` 目录，里面放签名发布包：`release-manifest.json`、`release-manifest.json.sig` 和清单列出的文件（`docker-compose.yml`、`niginx.conf`、`upgrade.sh`、`uninstall.sh`、`config/native/{prepare.py,runtime.encore.json,runtime.notification.json,tick.sh,README.md,update.py}`） | 安装器（`RAW_BASE`）和平台内“检查更新/升级”（`TP_RELEASE_SOURCE`）。两者可以是同一个地址 |
| **发布签名密钥** | ECDSA P-256 私钥，签 `release-manifest.json`；公钥就是**信任根** | 私钥只在发布机；公钥发给测试机 |
| **插件目录签名密钥** | 签 `plugin-catalog.json`（与上面是**两把不同的钥匙**，信任根已内置在 `update.py` 的 `PLUGIN_TRUST_PEM`） | 只在发布插件目录时用 |

> 现在**没有正式发布信任根**。beta 用一把你们自己生成的测试密钥；正式版换哪个文件见 §7。

## 1. 构建（一次，beta 和正式版共用）

1. **三样镜像/二进制**（交接文档 B1/B2）：
   * 后端镜像 `thingspanel-go:<TAG>`：`-ldflags "-X project/pkg/global.SYSTEM_VERSION=<TAG>"`，`<TAG>` 即 `vX.Y.Z-beta.N`。`./ThingsPanel-Go --version` 必须输出 `<TAG>`，Agent 靠它判断当前版本。
   * Encore 镜像：后端仓库 `.github/workflows/encore-image-build.yml` 的 `--services` 必须包含 `system`（后端分支 `wp/R8-ci-services` 修这个，合并前 Encore 镜像里没有 `system`，界面所有系统功能不可用）。
   * 前端镜像、Agent 二进制（amd64+arm64）。
   * **同一批构建**意味着：beta 通过后，正式版只是**给同样的镜像再打一个 `vX.Y.Z` 标签**（`docker buildx imagetools create -t <repo>:vX.Y.Z <repo>:vX.Y.Z-beta.N`，或 registry 的等价操作）。镜像内容不变，只有 `--version` 输出需要是正式版号——所以**正式版标签对应的镜像必须输出 `vX.Y.Z`**。这是一个已知取舍：若坚持“零重新构建”，后端 `--version` 不能由编译期写死，而要由启动参数/环境变量提供；目前代码是编译期注入，所以**正式版仍需要重新编译后端二进制（其余镜像可以复用）**，见 R9-DONE 的“需要负责人决定”。
2. 更新 `docker-compose.yml` 里各服务的镜像标签为 `<TAG>`（后端 `${TP_BACKEND_IMAGE:-…:<TAG>}`）。**安装器的校验要求 `--expected-version` 与 Compose 中后端镜像标签一致。**

## 2. 签名并发布到发布源

```bash
# 1) 一次性：生成 beta 测试密钥（私钥不进仓库，只放发布机）
openssl ecparam -name prime256v1 -genkey -noout -out release-beta.key
openssl ec -in release-beta.key -pubout -out release-beta.pem      # 这是信任根，发给测试机

# 2) 准备发布目录：把上面列出的文件按相对路径放进 DIR（来自你要发布的安装器提交）
# 3) 签名（--commit 是该安装器提交的 40 位 sha；--valid-days 清单有效期）
python3 tools/sign_release.py --payload DIR --key release-beta.key --version vX.Y.Z-beta.N --commit <40-hex> --valid-days 30
# 4) 把 DIR 的全部内容上传到 <SOURCE>/（HTTPS）
```

* 同一发布源**一次只放一个版本**（清单里的 `version` 就是“最新版本”）。发新版本 = 换目录内容；先传文件、最后传 `release-manifest.json.sig`。
* 本地先验：`python3 config/native/release_manifest.py --help`（用同一信任根验证目录）。
* 清单过期后“检查更新”会显示被拒；需要重新签名。

## 3. 安装 beta（测试机）

全新安装（一行命令保持不变，只多环境变量）：

```bash
export RAW_BASE=<SOURCE>                         # 安装包从这里取（默认是正式站；beta 必须改）
export TP_VERSION=vX.Y.Z-beta.N
export TP_RELEASE_TRUST_ROOT=/abs/path/release-beta.pem   # 校验安装包签名，同时成为平台内升级的信任根
export TP_RELEASE_SOURCE=<SOURCE>                # 不设则平台内“检查更新”显示“未配置发布源”
export TP_BUNDLE_PAGES_BASE=<SOURCE>             # 新装机器的 Agent/插件 bundle 也从 beta 发布源取（目录在 <SOURCE>/plugins/catalog-linux-<arch>.json）
export TP_BUNDLE_RELEASE_BASE=<ASSETS>           # 其下 <release_tag>/tp-agent-linux-<arch> 与 ha-image-linux-<arch>.tar；两个变量必须同时设置，均为 https
export TP_PLUGIN_TRUST_ROOT=/abs/plugin-test.pem # beta 目录用测试插件密钥签名时的插件信任根
bash install.bash                                # 安装器仓库里与本次发布同一提交的 install.bash（它不在签名清单里，但内部固定了 updater 摘要）
```

* 信任根文件必须在安装目录**之外**。updater 会把它复制到 `<安装目录>/.tp-agent/release/` 并记下 SHA-256；以后不必再设，除非要换（§7）。
* 私有 CA：`TP_RELEASE_CA=/abs/ca.pem`。离线/内网：`TP_UPGRADE_HELPER_IMAGE=<内网镜像>`，或按 §4 离线导入。
* `.env` 里写 `TP_RELEASE_SOURCE=`/`TP_UPGRADE_HELPER_IMAGE=` 也可（优先级低于环境变量）。
* 只有**全新安装**会下载 bundle；已有机器不会更换 Agent。beta 测试请用全新机器。
* 创建设备后立即跑 `upgrade.sh` 若撞上 Broker 范围同步，updater 会自动等待重试。
* **GitHub “latest release” 不包含预发布**：用 `upgrade.sh` 时必须显式写版本：`./upgrade.sh vX.Y.Z-beta.N`。
* 已有机器改配置/刷新 Agent：同一组环境变量再跑一次 `./upgrade.sh <当前版本>`（幂等，token 保留；Agent 以当前二进制重建，`system.json` 重新生成）。

## 4. 升级辅助镜像（离线机器）

升级用的 `docker:27-cli`（或你配置的镜像）要先在本机。install/upgrade 会尽力拉取；离线时：

```bash
# 有网的机器
docker pull docker:27-cli && docker save docker:27-cli -o helper.tar
# 目标机
docker load -i helper.tar
```

缺失时升级在**预检**就被拒绝（`upgrade_helper_image_unavailable`），尚未快照、停服或替换任何文件。

## 5. 怎么测

先看 Agent 侧（不依赖界面）：

```bash
# system 套接字/令牌在 <安装目录>/.tp-agent/system-api/
tp-agentctl -socket <安装目录>/.tp-agent/system-api/agent-system.sock -token-file <安装目录>/.tp-agent/system-api/token system updates
tp-agentctl … system status        # 健康、当前操作
tp-agentctl … system upgrade to=vX.Y.Z-beta.N
```

测试清单（beta 重点）：

1. “检查更新”：未配置发布源 → `updates_not_configured`；配置后 → `current/latest/newer` 正确（`beta.1 < beta.2 < 正式版`）。
2. 升级 `beta.N → beta.N+1`、`beta → 正式版`：先自动备份、维护页、完成后版本变化、数据不变。
3. 故意坏版本（迁移报错）→ 自动回退到升级前版本和数据。
4. 备份/恢复：旧版本备份可恢复到新版本；**更高版本的备份恢复到较低版本被拒绝**（对 beta 同样成立：`vX.Y.Z` 备份不能恢复到 `vX.Y.Z-beta.N` 机器）。
5. 交接文档 §4.2 的其余场景（重点 10、12、14、17、18）。

## 6. 提升为正式版（不重新构建）

1. 在**同一批镜像**上打正式标签（见 §1 的取舍说明），`docker-compose.yml` 的标签改为 `vX.Y.Z`。
2. 用**正式发布密钥**（或同一把 beta 密钥，若负责人决定）对 `--version vX.Y.Z` 重新签名同一提交的文件；上传到发布源（替换 beta 内容）。
3. beta 机器：`系统设置 → 检查更新` 应显示 `vX.Y.Z` 更新 → 升级。数据应保持不变。
4. 新装机器：`TP_VERSION=vX.Y.Z`。

## 7. 信任根：beta → 正式

* beta 机器信任的是**测试公钥**（钉在 `.tp-agent/release/`）。把发布换成正式密钥签名后，beta 机器上的升级会因签名不符被拒——这是设计如此，不是故障。
* 正式公钥一旦产生：把它放进 `config/native/update.py` 的常量 `OFFICIAL_RELEASE_TRUST_PEM`（与 `PLUGIN_TRUST_PEM` 同样做代码评审；之后重算 `UPDATE_ENGINE_SHA256` 并更新四个入口脚本，见 `UPDATE-SAFETY.md`）。
* 新装的正式机器不再需要 `TP_RELEASE_TRUST_ROOT`，自动使用内置公钥。
* beta 机器切到正式信任根：`TP_REPLACE_RELEASE_TRUST_ROOT=1 ./upgrade.sh <当前版本>`（不提供 `TP_RELEASE_TRUST_ROOT`，则恢复内置正式公钥；或同时给出正式公钥文件）。没有 `TP_REPLACE_RELEASE_TRUST_ROOT=1` 时，换一个不同的信任根会被拒绝（防止被悄悄替换）。

## 8. 插件目录与镜像（B4，最小方案）

新的签名插件目录从同一发布源取：`<SOURCE>/plugins/catalog-linux-<arch>.json` 和 `.json.sig`（`<arch>` 为 `amd64`/`arm64`；用**插件目录签名密钥**签，见 `tools/plugin_catalog.py publish`）。

```bash
python3 <安装目录>/config/native/update.py --root <安装目录> --sync-plugin-catalog \
        [--image-dir DIR] [--allow-missing-images]
```

* 先验签（与 Agent 相同），再逐条目确认镜像在本机；不在本机的从 `DIR/<插件id>-linux-<arch>.tar` 导入（校验 `image_archive_sha256`，导入后镜像 ID 必须等于目录里的 `image_id`）。任何镜像缺失则**不替换**目录（除非 `--allow-missing-images`）。
* 只接受“可直接替换”的目录：Agent 二进制摘要和 Home Assistant 镜像包摘要必须与当前 bundle 一致，目录版本不得回退。否则需要新 bundle（现行流程只在无 active bundle 时下载，见 R9-DONE“需要负责人决定”）。
* Agent 每次使用都会重新验签；两个文件依次替换，期间极短的一次读取会失败关闭。

## 9. 出问题怎么撤回

| 情况 | 做法 |
| --- | --- |
| 发布错了，还没人升级 | 把发布源目录换回上一版本（先传文件、最后传 `.sig`）。清单 `version` 变回旧版本，“检查更新”不再提示新版本 |
| 已发布的版本有严重问题 | 在受影响机器的 `TP_REVOKED_VERSIONS=vX.Y.Z-beta.N`（逗号分隔）再运行安装/升级入口，payload 校验会拒绝该版本；清单自身的 `revoked_versions` 字段也可列出被撤回版本，Agent 同样拒绝 |
| 升级中失败 | 自动回退到升级前的版本和数据（`upgrade_failed_rolled_back`）；回退也失败为 `needs_human`，保留所有备份，见交接文档 §4.2 #14 |
| 想手动回退到升级前 | 备份页恢复升级前自动创建的快照（版本号在备份名里） |
| beta 机器要回到旧正式版 | **不支持降级**：`beta → 旧正式版` 会被视为“不是更新”。用升级前的备份恢复同一版本，或重装 |
| 测试信任根泄露/换钥 | 新钥签发，机器上 `TP_REPLACE_RELEASE_TRUST_ROOT=1` 加新公钥 |
