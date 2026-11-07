# 龙道 AI 服务器部署实录

> 记录日期：2026-07-16
> 目标：把龙道 AI Token 中转平台从本地部署到日本 VPS，封闭内测上线
> 结果：`https://longdaoai.cn` HTTPS 正常访问，四容器健康

本文档是一次**真实、成功**的部署记录，命令可直接复用。适用于全新服务器从零部署。

---

## 一、部署前提

| 项 | 本次实际值 | 说明 |
|---|---|---|
| 服务器 | 日本 VPS，2核4G，40G，Debian 12 x64 | IP 干净、留后路 |
| 公网 IP | `64.83.39.223` | |
| 域名 | `longdaoai.cn` | 已解析 A 记录到公网 IP |
| 上游 | 第三方 AI 中转站 | 免维护，后台配置 |
| 本地 | Mac 终端 SSH | root 登录 |

**安全组必须放行入站端口：`22`(SSH)、`80`(HTTP+证书申请)、`443`(HTTPS)。**
国内云默认常常只开 22，需手动加 80/443，否则 HTTPS 无法签发。

---

## 二、完整部署步骤

### 第 1 步：SSH 登录服务器

Mac 终端：

```bash
ssh root@64.83.39.223
# 首次连接输入 yes；再输入 root 密码（输入时不显示字符，正常）
```

### 第 2 步：检查系统配置

```bash
uname -m && cat /etc/debian_version && free -h && df -h / && nproc
```

确认：x86_64 架构、Debian 12、内存/磁盘/核心数符合预期。

### 第 3 步：安装基础工具

```bash
apt-get update && apt-get install -y ca-certificates curl
```

### 第 4 步：安装 Docker

日本服务器直连 Docker 官方源无障碍：

```bash
curl -fsSL https://get.docker.com | sh
```

### 第 5 步：验证 Docker

```bash
docker --version && docker compose version && docker run --rm hello-world
```

看到 `Hello from Docker!` 即成功。

### 第 6 步：拉取代码（私有仓库）

用 GitHub Token 拉取（拉完立即清理 token）：

```bash
git clone https://<用户名>:<GitHub_Token>@github.com/yaoruiquan/LongdaoAI.git /opt/longdao
```

### 第 7 步：清理 token + 确认代码

```bash
cd /opt/longdao && \
git remote set-url origin https://github.com/yaoruiquan/LongdaoAI.git && \
git remote -v && git log --oneline -5 && ls deploy/production/
```

确认远程地址已不含 token、代码为最新、部署文件齐全。

### 第 8 步：构建自有版本镜像

编译前端 + 后端，打包成不可变版本镜像（约 5-10 分钟）：

```bash
cd /opt/longdao && ./deploy/build_production_image.sh v2026.07.15-1
```

产出 `longdao/sub2api:v2026.07.15-1`，映射到具体 git commit。

### 第 9 步：验证域名解析

```bash
getent hosts longdaoai.cn || nslookup longdaoai.cn
# 应返回服务器公网 IP，未生效则等几分钟 DNS 传播
```

### 第 10 步：生成 .env 配置（自动生成随机密钥）

一条命令：复制模板、填域名/邮箱、生成 4 个随机密钥 + 随机管理员初始密码：

```bash
cd /opt/longdao/deploy/production && \
cp -n .env.example .env && \
sed -i "s|^LONGDAO_DOMAIN=.*|LONGDAO_DOMAIN=longdaoai.cn|" .env && \
sed -i "s|^LONGDAO_ACME_EMAIL=.*|LONGDAO_ACME_EMAIL=你的邮箱|" .env && \
sed -i "s|^ADMIN_EMAIL=.*|ADMIN_EMAIL=你的邮箱|" .env && \
sed -i "s|^POSTGRES_PASSWORD=.*|POSTGRES_PASSWORD=$(openssl rand -hex 24)|" .env && \
sed -i "s|^REDIS_PASSWORD=.*|REDIS_PASSWORD=$(openssl rand -hex 24)|" .env && \
sed -i "s|^JWT_SECRET=.*|JWT_SECRET=$(openssl rand -hex 32)|" .env && \
sed -i "s|^TOTP_ENCRYPTION_KEY=.*|TOTP_ENCRYPTION_KEY=$(openssl rand -hex 32)|" .env && \
ADMINPW=$(openssl rand -hex 12) && \
sed -i "s|^ADMIN_PASSWORD=.*|ADMIN_PASSWORD=$ADMINPW|" .env && \
echo "===== 请立即私下保存以下登录信息 =====" && \
echo "访问地址: https://longdaoai.cn" && \
echo "管理员邮箱: 你的邮箱" && \
echo "管理员初始密码: $ADMINPW" && \
echo "===================================="
```

> ⚠️ 管理员初始密码务必私下保存（密码管理器）。数据库/Redis/JWT/TOTP 密钥已随机生成写入 `.env`，
> 无需人工干预。`.env` 含真实密钥，**绝不提交 git**（`.gitignore` 已排除）。

### 第 11 步：校验配置

```bash
cd /opt/longdao/deploy/production && docker compose config -q && echo "配置校验通过 ✅"
```

任何必填项缺失都会在此报错。

### 第 12 步：启动全部服务

```bash
cd /opt/longdao/deploy/production && docker compose up -d
```

首次启动会：拉取 postgres/redis/caddy 镜像 → 启动四容器 → 应用自动建库建管理员 →
Caddy 自动申请 Let's Encrypt 证书。

### 第 13 步：检查状态与健康

```bash
cd /opt/longdao/deploy/production && \
docker compose ps && \
docker compose exec -T sub2api wget -qO- http://localhost:8080/health && \
docker compose logs caddy | grep -i "certificate\|obtain\|error" | tail -10
```

期望：四容器 healthy、健康检查返回 `{"status":"ok"}`、日志出现
`certificate obtained successfully`。

### 第 14 步：公网 HTTPS 端到端验证

```bash
echo "=== HTTPS 健康检查 ===" && curl -s https://longdaoai.cn/health && echo "" && \
echo "=== HTTP 自动跳 HTTPS ===" && curl -sI http://longdaoai.cn | grep -i "location\|HTTP/" && echo "" && \
echo "=== 证书信息 ===" && echo | openssl s_client -connect longdaoai.cn:443 -servername longdaoai.cn 2>/dev/null | openssl x509 -noout -issuer -dates
```

期望：HTTPS 返回 `{"status":"ok"}`、HTTP 返回 308 跳转、证书由 Let's Encrypt 签发。

**到此，浏览器访问 `https://longdaoai.cn` 即可看到龙道 AI 页面。**

---

## 三、已知问题与修复

### Caddy 容器显示 unhealthy（不影响服务）

**现象**：`docker compose ps` 中 caddy 显示 `unhealthy`，但证书正常、HTTPS 可访问。
健康检查日志报 `wget: can't connect to remote host: Connection refused`。

**原因**：健康检查用 `http://localhost:2019/config/` 探活 Caddy admin API，
但 busybox 的 `wget` 把 `localhost` 优先解析为 IPv6 `::1`，而 Caddy admin API
只监听 IPv4 `127.0.0.1:2019`，导致连接被拒。

**修复**：把健康检查地址的 `localhost` 改为 `127.0.0.1`（已在 `docker-compose.yml` 修复）。

服务器上应用修复：

```bash
cd /opt/longdao && git pull && \
cd deploy/production && docker compose up -d caddy && \
sleep 20 && docker compose ps
```

修复后 caddy 应显示 `healthy`。

---

## 四、上线后必做

### 1. 首次登录改密码

浏览器打开 `https://longdaoai.cn`，用管理员邮箱 + 初始密码登录，**立即改成自己的强密码**，
并启用 TOTP 双因素认证。

### 2. 配置上游账号（对接中转）

管理后台 → 账号管理 → 添加上游账号，填入你购买的中转站 API 地址和 Key。

### 3. 确认注册开关

管理后台 → 系统设置 → 注册开关。封闭内测建议**关闭注册**，由管理员手动建用户。
若要开放自助注册则打开。

### 4. 给用户发额度

封闭内测阶段用**兑换码**或**管理员手动加余额**（在线支付尚未接入）。

---

## 五、日常运维命令

```bash
cd /opt/longdao/deploy/production

# 查看状态
docker compose ps

# 查看日志
docker compose logs -f sub2api caddy

# 重启某个服务
docker compose restart sub2api

# 停止（保留数据）
docker compose down

# 数据库备份（脚本已内置）
./backup.sh

# 发布新版本（备份→迁移→启动→健康→冒烟）
./deploy.sh

# 回滚到上一版本
./rollback.sh <旧版本tag>
```

详见 `deploy/production/OPS_README.md`（运维手册）和 `README.md`（部署说明）。

---

## 六、当前上线模式

**模式 A：封闭内测**（当前）

- 注册可控（建议关闭，管理员建号）
- 余额靠兑换码 / 手动发放
- 在线支付未接入（充值按钮明确显示"暂不可充值"）
- API 中转、用量记录、余额扣费全部可用

**模式 B：公开收费**（未来，需先开发）

- 支付订单 + 回调验签 + 资金账本 + 对账 + 风控
- 详见 `docs/specs/2026-07-15-longdao-server-launch-spec.md` 第 7 章

---

*部署实录记录于 2026-07-16，基于版本 `v2026.07.15-1`（commit db206b60）*
