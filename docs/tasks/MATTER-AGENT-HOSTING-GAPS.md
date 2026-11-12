# Agent 托管 Matter：缺口清单（第 1 步，只读盘点）

日期：2026-11-12　对象：`thingspanel-matter-controller:candidate-platform-map-20261112`（`sha256:69d1aaa7…`）
方法：对运行中的外部容器实测（`docker stats/inspect/top`、`/proc`、cgroup），并对照 Agent 写死的参数（`agent/docker_prepare.go`、`prepare.go`）。**未改任何代码。**
注意：被测容器里还带着测试用的 lighting 模拟器（约 4 MiB、1 个任务），数字略偏高。

## 实测
| 项 | 结果 |
|---|---|
| 内存 | 当前 82 MiB；cgroup 峰值约 125 MiB；Python 控制器 RSS 74 MiB |
| 进程/线程（pids 计数含线程） | 约 20：`/connector` 11、Python 4、runuser/dbus/avahi 各 1、入口 sh 等 |
| 启动用户 | 入口脚本以 **root** 启动，最后 `runuser` 降到 UID 10001 |
| 需要 root 的动作 | `dbus-daemon --system`、`avahi-daemon --daemonize --no-chroot`、写 `/run/dbus`、`/var/lib/dbus`、`/run/avahi-daemon`、`chown` 存储目录、`runuser` |
| 根文件系统 | 可写（Agent 要求只读） |
| 存储 | `/var/lib/thingspanel-matter`（Fabric，持久）、`/tmp`（tmpfs，SDK 临时文件） |
| 网络 | 需要到设备的 IPv6/mDNS 组播；模拟器验收用的是双网络命名空间变通 |
| 镜像自身合规 | 通过：无 labels/volumes/onbuild；`EXPOSE 9001` 在 `ReviewedImageMetadata` 下可接受 |

## 对照 Agent 当前写死的参数
| 参数 | Agent 现在 | Matter 需要 | 结论 |
|---|---|---|---|
| 内存 | 64 MiB | ≥256 MiB（清单自己写的 memoryLimit 256Mi；实测峰值 125 MiB） | **缺口**：需目录可声明、Agent 设上限 |
| CPU | 0.1 核 | 0.5 核（清单） | **缺口**：同上 |
| pids | 16 | ≥64（实测约 20，留余量） | **缺口**：同上 |
| 用户 | 固定 65534 | 现在：root 起、降到 10001 | **冲突**：见下 |
| 能力 | `CapDrop ALL` + `no-new-privileges` | dbus/avahi/chown/runuser 需要 root 及若干 capability | **冲突**：见下 |
| 根文件系统 | 只读 | 写 `/run`、`/var/lib/dbus`… | 用已做的 tmpfs 可解一部分；去掉 dbus/avahi 后基本不需要 |
| 卷 | 已支持（本分支） | Fabric 目录，需属主与运行用户一致 | 镜像需把目录属主设为运行用户 |
| 网络 | 单个用户自定义网络 + 别名 | LAN 组播/IPv6 | **缺口**：C 项，需受审的 LAN 网络能力 |
| 安装 ID | 已定约定 | 租户唯一 | 控制面传递仍待做 |

## 关键决策：root / D-Bus / Avahi
Agent 的安全立场是"非 root + 丢全部能力 + 只读根"。Matter 现在的镜像依赖 root 才能起 D-Bus 与 Avahi（`chip_mdns=platform`，见 `MATTER-SDK.lock`）。两条路：
- **A. 改镜像（推荐方向）**：让 SDK 不再依赖 Avahi/D-Bus（例如 `chip_mdns=minimal`，进程内组播），整个服务以非 root 运行、只读根、几乎无需 capability。**未验证**：该配置下 Python 控制器的发现与入网是否可用，需要重新编译 SDK（ARM64 VM）并用模拟器验证。
- **B. 放宽 Agent**：增加"允许 root + 指定 capability"的声明。会削弱 Agent 对所有插件的隔离，且 avahi 的 `--no-chroot`、降权都需要特定 capability；不建议。

## 建议顺序
1. 先做 A 的可行性验证（SDK 重编译 + 模拟器），再决定 Agent 要加什么。
2. Agent 增加**有上限**的资源声明（内存/CPU/pids），不放开用户和能力。
3. 目录条目补 volumes/tmpfs/资源，经"插件安装"页安装。
4. LAN 网络能力单独评审（C 项）。
