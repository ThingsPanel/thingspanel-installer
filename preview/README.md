# 通知与 Encore 隔离预览

本目录用于构建和启动固定版本的测试包。它不会发布产品版本。请先阅读 [同事测试指南](../docs/notification-preview-testing.md)。

## 构建

构建机器需要 Python 3.12、Go、pnpm、Encore CLI 1.58.6、Docker Engine 及 Docker Compose >= 2.30；构建期间需要联网下载依赖和基础镜像。源码仓库必须干净，提交必须与 `source-lock.json` 完全一致。Core 是私有仓库，需要访问权限。

在安装器仓库执行，输出目录必须不存在：

```bash
python3 preview/build.py \
  --backend /path/to/thingspanel-backend-community \
  --frontend /path/to/thingspanel-frontend-community \
  --core /path/to/iot-api-encore-kernel \
  --smtp /path/to/thingspanel-notification-smtp \
  --encore-bin /path/to/encore-1.58.6 --arch arm64 --output /path/to/new-preview-bundle
```

`--arch amd64` 是构建选项，当前没有 amd64 实测交付。每个源码仓库只从 Git 提交归档，未提交改动和忽略的私密文件不进入包。镜像独立压缩以控制临时磁盘占用；归档校验、镜像 ID、架构和源提交均写入包的 `offline-images.lock.json`。

## 同事运行

运行机器需要 Python 3.12、Docker 和 Compose >= 2.30。在包根目录执行：

```bash
bash preview/start.sh
```

页面地址为 `http://127.0.0.1:28080`，物模型测试 API 为 `http://127.0.0.1:29407/api/device-metadata/healthz`。远程测试机通过 SSH 端口转发访问页面，例如 `ssh -L 28080:127.0.0.1:28080 tester@test-host`；不要把数据库、插件或管理接口开放到公网。

安装检查成功后，升级同一预览环境：

```bash
bash preview/upgrade.sh
```

校验器会在安装前拒绝不匹配的镜像、归档、数据卷、项目、外部目录或冲突网段。包生成时选择一个构建主机上空闲的私有网段；换到同事主机时仍会重新检查。首次安装前可选择空闲网段；安装后不能改网段，也不能复用产品数据卷。

SMTP 服务默认一起安装并登记。SMTP 账号、租户服务授权和通知策略仍由对应管理员配置；空出站允许列表不会发送邮件。测试包不附带真实密码、私钥、租户数据或邮件凭据。

验收成功后再通过代码审查合入发布主分支、生成正式版本。不要直接从预览分支打产品 `v*` 标签或部署生产环境。
