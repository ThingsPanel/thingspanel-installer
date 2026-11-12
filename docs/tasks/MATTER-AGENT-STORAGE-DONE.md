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
- 真实 Docker 只覆盖到 create-only 的回读；`ro` 卷、多卷、容器真正启动后卷内数据持久（重启/换代后 Fabric 恢复）没有在真实 Docker 里验证。已有的 3 个真实 Docker *启动* 用例（`activate_real_test.go`）需要可运行镜像；用我这个不可执行的测试镜像会 start 400，不是代码回归，未用可运行镜像重跑。
- 卷属主：Agent 固定 `65534:65534` 运行，首次挂载依赖镜像里目标目录已按该 UID 建好（Docker 会拷贝属主）；Matter 镜像需满足，或另行约定。
- 控制面如何生成并传递租户安装 ID（后端/安装、升级、授权、密钥、卸载流程）**未改**，这里只定了 Agent 侧约定。
- 卸载保留数据已是默认（Agent 无删卷接口）；**显式 purge 操作未实现**。
- 网络别名仍按 catalog ID 固定（`tp-plugin-<id>`），同一插件多租户同时运行会别名冲突，需另议。
- 发布/校验工具（Python publisher、`sign_catalog.sh`）尚未支持新字段。
- 双写拒绝依赖现有的单实例资源占用与 revoke→stop→才 start 的流程，未新增专门的卷级测试；未知 stop 结果 fail closed 沿用现有 `ErrActivationUnresolved`。
- C（网络/mDNS）、D（实体设备）、A（前端 UI）均未涉及。
