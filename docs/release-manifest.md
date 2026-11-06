# 签名 release manifest 验签（已接入 bash/PowerShell 入口的更新事务；默认未启用）

`config/native/release_manifest.py` 在使用任何 payload 之前验证一份已签名的 release manifest。

## 做了什么

- 信任锚只有调用方给出的 ECDSA P-256 公钥（PEM）。manifest 和 payload 都不能引入或替换它。
- 仓库**不内置任何根公钥**。未配置、文件缺失、不是公钥或不是 P-256 时，一律以 `trust_root_not_configured` 拒绝（fail closed）。
- 签名为对 manifest 原始字节的 SHA-256 做 DER ECDSA：`openssl dgst -sha256 -sign key.pem manifest.json > manifest.json.sig`。
- 签名有效还不够，必须同时满足：发布者等于期望值、版本符合、在有效期内、不在撤回列表、
  payload 目录里的文件与 manifest 完全一致（路径、长度、SHA-256），没有缺文件、多文件、符号链接，
  路径必须是干净的相对路径。
- manifest 在 payload 冻结后签名，payload 不内嵌 manifest 的摘要，避免 installer、updater 与管理脚本循环互相摘要。

## 接入方式

- `update.py` 新增 `--release-trust-root PEM`、`--require-signed-release`、`--release-publisher`、`--revoked-version`。
  暂存全部受管文件之后、校验和应用之前，从 `<base-url>/release-manifest.json` 与 `.sig` 取 manifest，
  逐个核对暂存文件（路径、长度、SHA-256）；任一不符，在改动任何已安装文件之前终止。
- 验签器 `release_manifest.py` 的 SHA-256 固定在 `update.py` 内，只在摘要一致时加载（旁边没有就按 base-url 下载后再核对）。
  入口脚本固定的仍是 `update.py` 摘要，因此链条是：入口 → `update.py` → 验签器 → 签名 manifest。
- 入口脚本从环境变量传入：`TP_RELEASE_TRUST_ROOT`（公钥路径，必须带外提供，不能来自 payload）、
  `TP_REQUIRE_SIGNED_RELEASE=1`（没有可用信任根则失败）、`TP_REVOKED_VERSIONS`（逗号分隔）。
  `install.bash`、`upgrade.sh`、`install.core.ps1`、`upgrade.ps1` 均已传递。
- 插件 catalog 有独立信任锚：`TP_PLUGIN_TRUST_ROOT` 传给 `update.py --plugin-trust-root`，必须是安装目录外的绝对公钥路径；安装器先验 catalog，再在启动后端前启动 Agent。插件包不能自带并替换这个根。当前仓库没有正式插件信任根或正式签名 catalog。
- 当前 `plugins/homeassistant.json` 已对齐候选 `1.0.0-candidate.20261106-4d8d45b`：`linux/arm64`、镜像 ID `sha256:be2578ecf78a37d4477f43a63685cbf1d137370f2f7de54ff11a726dd88d165e`。该条目不是可安装签名目录；候选镜像包在工作目录外，尚缺正式 catalog 签名和匹配的包外官方公钥。不得用历史 `t7c` 镜像或测试密钥发布。
- **默认行为不变**：没有配置信任根且未要求签名时，仍只靠入口对 `update.py` 的摘要固定，并向 stderr 打印警告。
  在发布负责人提供正式信任根并启用 `TP_REQUIRE_SIGNED_RELEASE` 之前，这不是签名信任链。
- 撤回列表目前来自 manifest 自带的 `revoked_versions`（已签名）和 `--revoked-version`；独立的撤回源尚未设计。

## 没做什么

- **没有正式发行信任根**：需要发布负责人提供公钥并决定保管方式；测试里的密钥只在每次测试的临时目录里生成。
- **PowerShell 入口只是传参，未在原生 Windows 上运行**；验签本身由 Python 完成，依赖 `openssl` 可用。
- 没有 TUF 式目录版本/重放保护，也没有信任根轮换；这些在公开市场上线前仍需单独设计。

## 验证

```sh
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest tests.test_release_manifest tests.test_signed_update -v
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s tests -p 'test_*.py'
```

覆盖：有效发布、无信任根、非公钥/RSA 根、他人密钥签名、篡改 manifest、他人发布者、版本不符、过期/未生效、
撤回、改动文件、缺文件、多文件（混发）、符号链接、路径穿越与绝对路径、重复路径与非法 commit、命令行退出码。

事务层集成测试（`tests/test_signed_update.py`）：有效签名发布被应用；服务端文件被篡改、他人密钥、清单漏列文件、撤回、过期、他人发布者、
要求签名却无信任根、验签器被替换均在改动任何已安装文件之前失败；孤立的 `update.py` 会下载并核对验签器；未配置时行为不变。
