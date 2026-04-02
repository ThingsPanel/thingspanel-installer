# 故障排查

## 常见问题

### 1. 安装后界面无法访问 (http://localhost:8080)

**原因 A：服务还在启动中**
```bash
# 查看服务状态，等待所有显示 (healthy)
docker compose -f /opt/thingspanel/docker-compose.yml ps

# 查看启动日志
docker compose -f /opt/thingspanel/docker-compose.yml logs backend
```

**原因 B：端口被占用**
```bash
# 检查 8080 端口
lsof -i :8080        # macOS/Linux
netstat -ano | findstr :8080    # Windows

# 修改端口（编辑 .env，改 HTTP_PORT=9090，然后重启）
docker compose -f /opt/thingspanel/docker-compose.yml up -d
```

---

### 2. 数据库连接失败（backend 反复重启）

```bash
# 查看 postgres 状态
docker compose -f /opt/thingspanel/docker-compose.yml ps postgres
docker compose -f /opt/thingspanel/docker-compose.yml logs postgres

# 手动测试连接
docker compose -f /opt/thingspanel/docker-compose.yml exec postgres \
    pg_isready -U postgres -d ThingsPanel
```

常见原因：
- 数据目录权限问题：`ls -la /opt/thingspanel/data/postgres`
- 磁盘空间不足：`df -h`

---

### 3. MQTT 设备无法连接

```bash
# 检查 gmqtt 是否正常
docker compose -f /opt/thingspanel/docker-compose.yml logs gmqtt

# 测试 MQTT 连接（需要安装 mosquitto-clients）
mosquitto_pub -h localhost -p 1883 -t test -m "hello" -u your_device_id -P your_token
```

---

### 4. Windows — Docker 未运行

1. 打开 Docker Desktop 等待完全加载（系统托盘出现鲸鱼图标）
2. 重新运行安装程序

---

### 5. 如何查看所有服务的实时日志

```bash
# 所有服务
docker compose -f /opt/thingspanel/docker-compose.yml logs -f

# 单独某个服务
docker compose -f /opt/thingspanel/docker-compose.yml logs -f backend
docker compose -f /opt/thingspanel/docker-compose.yml logs -f postgres
docker compose -f /opt/thingspanel/docker-compose.yml logs -f gmqtt
```

---

### 6. 如何重置密码

编辑 `/opt/thingspanel/.env`，修改相应密码后重启：

```bash
docker compose -f /opt/thingspanel/docker-compose.yml up -d
```

> 注意：修改数据库密码需要同时修改 POSTGRES_PASSWORD，并重建数据库（会丢失数据）。

---

### 7. 磁盘空间不足

```bash
# 查看 Docker 占用
docker system df

# 清理未使用的资源（不影响当前运行服务）
docker system prune -f
```

---

## 获取支持

- GitHub Issues：https://github.com/ThingsPanel/all-in-one-assembler/issues
- 社区论坛：https://thingspanel.io/community
