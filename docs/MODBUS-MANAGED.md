# 受管 Modbus 安装

插件安装目录使用一个 `modbus` 条目。Agent 启动一个容器，社区后端登记 `MODBUS_TCP` 和 `MODBUS_RTU`，已有协议记录 ID 和自定义字段也会保留。容器的 TCP 网关入口为 `tp-plugin-modbus:1502`，表单回调和健康检查走 `tp-plugin-modbus:1503`。

## 制品

1. 将受管版源码提交到 Modbus 仓库，记录完整提交号。社区后端需要同时包含受管 Modbus 的双协议登记与运行时授权。
2. 在安装器执行 `Build signed Modbus managed plugin`。传入 Modbus 源码提交，以及当前目标环境使用的 HA/HTTP 签名制品所在 Release 或 HTTP 工作流运行 ID。
3. 核对 amd64 和 arm64 输出。工作流使用既有官方信任根验证基线，对照签名目录中的摘要检查 Agent、HA 和 HTTP 制品，构建 Modbus 镜像并重新签名目录。激活前逐项对比当前 HA、HTTP 条目和 Agent 摘要，确保与候选一致。
4. 将对应架构的制品按现有 Agent 目录流程发布和加载。正式发布时，完整制品归入目录 `release_tag` 对应的 GitHub Release。公共一键安装的默认目录有自己的发布流程。

## 主线测试服准备

- 主线 Gin 运行在宿主机，需要与 HA、HTTP 相同的回调代理。`tools/main47/modbus_callback.go` 编译为 Linux amd64 二进制，放入 `modbus-runtime/modbus-callback`，执行 `prepare_modbus_callback.py --sha256 <二进制摘要>`。代理只发布 `127.0.0.1:1503`，宿主机别名 `tp-plugin-modbus` 指向回环，再经现有容器网络访问受管插件。

- Agent 使用的主机栈配置需提供 `TP_MODBUS_MQTT_USER` 和 `TP_MODBUS_MQTT_PASSWORD`。主线测试服的专用用户名为 `tp-main47-modbus`。同步使用同一份私有凭据配置 `modbus-runtime/mqtt.private.json`。
- 将 `tools/main47/modbus_scope.py` 放入 `modbus-runtime/scope.py`，部署同目录的 `modbus-scope.service` 和 `modbus-scope.timer`。它在安装代次有效时配置专用 Broker 账号，只授予符合条件的 Modbus 网关及其同租户、同协议子设备。代次停用时移除账号。范围实际改变时会重启现有 Broker Owner 和 Mosquitto，启用定时器前确认该环境允许短暂重连。
- 已有设备 TCP 入口应经受控 TCP 转发指向容器的 `1502`。切换前确认旧 Modbus 的监听端口和设备流量，验收时使用独立测试入口；平台内部的 `1503` 供回调和健康检查使用。

## 验收

从插件安装页点一次安装，核对一张 Modbus 卡片、一个健康容器和两个协议记录。分别用测试 TCP、RTU 网关及其子设备确认接入、采集入库、遥测控制写入和读回。验证其他协议及跨租户子设备查询被拒绝，停用后旧令牌和 MQTT 账号失效。重新启用后原网关可重连，原 HA、HTTP 继续可用。
