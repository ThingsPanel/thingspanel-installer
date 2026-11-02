# 签名 release manifest 验签（机制，未接入安装入口）

`config/native/release_manifest.py` 在使用任何 payload 之前验证一份已签名的 release manifest。

## 做了什么

- 信任锚只有调用方给出的 ECDSA P-256 公钥（PEM）。manifest 和 payload 都不能引入或替换它。
- 仓库**不内置任何根公钥**。未配置、文件缺失、不是公钥或不是 P-256 时，一律以 `trust_root_not_configured` 拒绝（fail closed）。
- 签名为对 manifest 原始字节的 SHA-256 做 DER ECDSA：`openssl dgst -sha256 -sign key.pem manifest.json > manifest.json.sig`。
- 签名有效还不够，必须同时满足：发布者等于期望值、版本符合、在有效期内、不在撤回列表、
  payload 目录里的文件与 manifest 完全一致（路径、长度、SHA-256），没有缺文件、多文件、符号链接，
  路径必须是干净的相对路径。
- manifest 在 payload 冻结后签名，payload 不内嵌 manifest 的摘要，避免 installer、updater 与管理脚本循环互相摘要。

## 没做什么

- **没有正式发行信任根**：需要发布负责人提供公钥并决定保管方式；测试里的密钥只在每次测试的临时目录里生成。
- **尚未接入** `install.sh` / `install.bash` / `upgrade.sh` / `*.ps1`：这些入口目前仍只固定 updater 的 SHA-256。
  接入需要信任根、manifest 下载位置、撤回列表来源和 updater 变更授权的决定。
- **没有 PowerShell 实现，也没有在原生 Windows 上运行**。
- 没有 TUF 式目录版本/重放保护，也没有信任根轮换；这些在公开市场上线前仍需单独设计。

## 验证

```sh
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest tests.test_release_manifest -v
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s tests -p 'test_*.py'
```

覆盖：有效发布、无信任根、非公钥/RSA 根、他人密钥签名、篡改 manifest、他人发布者、版本不符、过期/未生效、
撤回、改动文件、缺文件、多文件（混发）、符号链接、路径穿越与绝对路径、重复路径与非法 commit、命令行退出码。
