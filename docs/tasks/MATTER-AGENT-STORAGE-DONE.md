# Agent 通用卷（Matter 前置）交付说明

日期：2026-11-12　分支：`codex/matter-agent-generic-storage`（基于 `dea71d8`，未提交、未推送）

## 做了什么（均在 `agent/`）
- **安装 ID 约定**：runtime instance 为 `<catalogID>.<installID>`；裸 `<catalogID>` 仍是旧的单例安装。`SplitInstance`、`validInstallID`（`catalog.go`）。Build 与 `declaredSecret` 都按此拆分。
- **签名目录新增字段**：`volumes[{key,destination,mode rw|ro}]`、`tmpfs[{destination,size_mib}]`。校验：绝对且已 clean 的路径、拒绝 `/`、`/etc` `/proc` `/sys` `/dev` `/run` `/usr` 等保留前缀、拒绝 `, : \`、key 唯一、路径互不嵌套、最多 4 卷 2 tmpfs、tmpfs 1–256 MiB。不允许 bind 源路径、驱动或任意选项。
- **卷身份**：`tp-agent-vol-` + hash(host, instance, key)。instance 含租户安装 ID，所以租户之间不同；**不含 generation**，升级/回滚复用同一卷。
- **单例保护**：目录声明了卷、但 instance 没有安装 ID 时，Build 拒绝。
- **profile digest 与回读**：`PreparationProfile.Mounts/Tmpfs` 进入 digest。创建请求用 `HostConfig.Mounts`（volume 类型）和 `Tmpfs`（`rw,noexec,nosuid,nodev,size=Nm`）。回读 `safeStorage` 精确比对：无 Binds、挂载集合完全一致（类型、源、目标、只读）、无额外驱动选项、`container.Mounts` 的卷名/RW/driver=local、tmpfs 一致；默认无声明时仍然拒绝任何挂载。
- 测试：`agent/storage_test.go`。

## 怎么验证的
`docker run golang:1.22`（Linux）里 `go vet ./...` 和 `go test -race -count=1 ./...` 通过。
单元/夹具层之外，新增真实 Docker 用例 `TestPreparationRealDockerNamedVolumeAndTmpfsReadback`（OrbStack，`TP_AGENT_REAL_DOCKER=1`，本地 `docker import` 的极小镜像，无 pull/build）：真实创建带命名卷 + tmpfs 的容器，回读被接受，重开 ledger 后 Query 仍为 prepared，测试结束删除自己创建的容器与卷。
这个用例抓到一个真实偏差：Docker 在 `ReadOnly=false` 时会省略该键，我最初“恰好 4 个键”的检查误拒合法容器，已改为只允许 Type/Source/Target/ReadOnly。真实回读形状：`Mounts[].Driver=local, Mode=z, RW=true`；`HostConfig.Mounts=[{Type,Source,Target}]`；`Tmpfs={"/tmp":"rw,noexec,nosuid,nodev,size=16m"}`；`Config.Volumes=null`。

## 没做 / 未验证
- 真实 Docker 只覆盖到 create-only 的回读；`ro` 卷、多卷、容器真正启动后卷内数据持久（重启/换代后 Fabric 恢复）没有在真实 Docker 里验证。
- 卷属主：Agent 固定 `65534:65534` 运行，首次挂载依赖镜像里目标目录已按该 UID 建好（Docker 会拷贝属主）；Matter 镜像需满足，或另行约定。
- 控制面如何生成并传递租户安装 ID（后端/安装、升级、授权、密钥、卸载流程）**未改**，这里只定了 Agent 侧约定。
- 卸载保留数据已是默认（Agent 无删卷接口）；**显式 purge 操作未实现**。
- 网络别名仍按 catalog ID 固定（`tp-plugin-<id>`），同一插件多租户同时运行会别名冲突，需另议。
- C（网络/mDNS）、D（实体设备）、A（前端 UI）均未涉及。

## 补充（同日，真实启动层）
- `TestActivationVolumeSurvivesTakeoverAndIsPerTenantRealDocker`（真实 Docker，真实 start）：租户 A 第 1 代写入卷 → 第 2 代未退役前 Prepare 被拒（`ErrResourceConflict`，单写者）→ Sweep+Retire 后第 2 代启动，卷内日志为 `n=0,n=1`（数据跨代保留）→ 租户 B 得到独立空卷 `n=0`。用的是本地 `docker import` 的惰性 stub 镜像（含 `/pluginstub`，写 `/data/log`），不是 Matter 镜像。整个 agent 包（含全部真实 Docker 启动用例）`-race` 通过，测试后无遗留容器/卷。
- 发布工具：`tools/plugin_catalog.py` 新增 `check_storage`（与 Agent 同规则：禁止 bind 源/驱动/选项、保留路径、嵌套、tmpfs 1–256 MiB）与 `tools/test_plugin_catalog_storage.py`。`test_plugin_bundle`/`test_plugin_install` 有 4 个失败 2 个错误，**基线 `dea71d8` 上相同**，与本次无关。
- 单写者与"未知 stop 结果 fail closed"沿用现有 Activator 机制，真实 Docker 里验证了前者。

## 重要发现：Agent 目前托管不了 Matter 镜像（不止缺卷）
`docker_prepare.go` 对所有预备容器固定：用户 `65534:65534`、只读根文件系统、内存 64 MiB、PID 上限 16、`CapDrop ALL`。Matter 镜像需要 D-Bus + Avahi + Python Controller，以 UID 10001 运行，且需要局域网 IPv6/mDNS 组播。因此：卷能力是必要但不充分条件；还需要可声明的资源上限、运行用户（或镜像适配 65534）、以及受审的 LAN 网络能力（C 项）。这些**未做**。
- 控制面传租户安装 ID：安装器 `tools/plugin_install.py` 仍以 `entry['id']` 作 instance；平台运行时身份 API 的 `plugin_instance` 是否接受 `<id>.<installID>` 未验证（改平台源码超出范围）。
- purge（显式删卷）未实现。
- 网络别名仍按 catalog ID 固定。
