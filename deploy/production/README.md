# 龙道 AI 生产部署（Docker Compose + 容器化 Caddy）

**2026-11-03 华为云迁移后，正式部署与运维请使用 [HUAWEI.md](HUAWEI.md)。** 目标为 `121.37.128.126` 的 `/www/longdao`，使用独立 Docker；本页后续的传统 Compose/脚本步骤保留作旧部署参考，不能直接用于新机。

本目录是龙道 AI 的**生产环境**部署清单，相较上游 `deploy/docker-compose.yml` 做了安全与稳定性收敛：

- 应用镜像固定为自有版本化 tag（禁止 `latest`）。
- 应用**不再直接暴露公网**，对外只经容器化 Caddy 反向代理（HTTPS）。
- PostgreSQL / Redis 均不映射宿主机端口，只在内部网络可达。
- 每个服务都加了 CPU/内存限制与日志轮转，防止资源与磁盘被打爆。
- Caddy 自动签发 Let's Encrypt 证书，HTTP 自动跳转 HTTPS。
- **蓝绿部署（零中断发布）**：应用拆为 `sub2api-blue` / `sub2api-green` 两色实例，
  发布时先起新色、健康+冒烟通过后再切流、最后停旧色，全程至少一个健康实例在线。
  配合应用侧优雅关闭（SIGTERM → 60s drain 在途流式请求），实现不断流升级。

## 目录内容

| 文件 | 说明 |
|------|------|
| `docker-compose.yml` | 生产 compose：`sub2api-blue`/`sub2api-green`（蓝绿）+ `postgres` / `redis` / `caddy` |
| `Caddyfile` | 容器化 Caddy 反向代理（HTTPS + 流式友好 + 蓝绿双 upstream 健康检查） |
| `.env.example` | 环境变量模板（只含占位符，复制为 `.env` 后填真实值） |
| `_active.sh` | 蓝绿共享辅助（被其他脚本 source；维护 `.active_color` 活跃色状态） |
| `deploy.sh` | 一键发布编排：检查 → 备份 → 迁移 → 蓝绿切换 → 记录 |
| `switch.sh` | 蓝绿切换核心：起目标色 → 健康 → 冒烟 → 放量 → 停旧色 |
| `rollback.sh` | 零中断回滚：用旧 tag 部署到目标色并蓝绿切换 |
| `migrate.sh` / `backup.sh` / `restore.sh` | 迁移 / 备份 / 恢复运维脚本 |
| `smoke-test.sh` | 发布后冒烟（P0 健康/首页/注册策略） |
| `.active_color` | （运行时产物，不入库）当前活跃色 `blue`/`green` |
| `README.md` | 本文档 |

## 前置条件

1. 一台可从公网访问的服务器，已安装 Docker（含 Compose v2）。
2. 一个**已解析到本机公网 IP 的域名**（Let's Encrypt 需要通过 80/443 完成域名验证）。
3. 防火墙放行入站 **80 与 443**;这是唯一需要对公网开放的端口。
4. 已用 Release workflow 发布版本化镜像（例如 `ghcr.io/yaoruiquan/sub2api:2026.07.15-1`），
   并且部署机能拉到/已有该镜像。

## 快速开始

> **共享基础设施警告**：生产服务器上的 PostgreSQL、Redis、Caddy 同时服务龙道和 SEP。
> 只有空白服务器首次安装时才执行下面的基础设施启动命令；已有环境禁止用本目录执行
> `docker compose up -d postgres redis caddy`、`docker compose down` 或强制重建 Caddy。
> 日常发布只操作 `sub2api-blue` / `sub2api-green`，数据库迁移使用 `--no-deps`。

```bash
cd deploy/production

# 1) 生成环境文件并填写
cp .env.example .env
#   必填项：IMAGE_TAG、LONGDAO_DOMAIN、POSTGRES_PASSWORD、REDIS_PASSWORD、
#          ADMIN_EMAIL、ADMIN_PASSWORD、JWT_SECRET、TOTP_ENCRYPTION_KEY
#   随机密钥用：openssl rand -hex 32

# 2) 校验配置（缺任何必填项都会在此报错；蓝绿需带两色 profile）
docker compose --profile blue --profile green config -q

# 3) 首次启动（仅首次安装）：起共享服务 + 首个活跃色 blue
#    日常发布不会管理共享 PostgreSQL、Redis、Caddy 的生命周期。
docker compose up -d postgres redis caddy
docker compose --profile blue up -d sub2api-blue
echo blue > .active_color        # 记录当前活跃色（switch.sh 之后会自动维护）

# 4) 观察日志
docker compose logs -f sub2api-blue caddy

# 5) 访问
#    https://<你的 LONGDAO_DOMAIN>
```

首次启动时 `AUTO_SETUP=true` 会自动建表并创建管理员账号（用 `.env` 里的 `ADMIN_EMAIL` /
`ADMIN_PASSWORD`）。登录后请尽快修改初始密码。

> **注意**：蓝绿下不要用裸 `docker compose up -d`——它会同时拉起 blue 和 green 两个实例。
> 平时只跑一个活跃色；日常发布一律走 `deploy.sh`（内部调 `switch.sh` 自动在两色间切换）。

## 首次从旧单实例迁移到蓝绿（一次性，重要）

若服务器此前跑的是旧单实例容器（service `sub2api`，容器名 `longdao-sub2api`），
升级到本蓝绿版本时需要一次手动切换（之后就全走 `deploy.sh`）：

```bash
cd deploy/production
git pull                              # 拉到蓝绿版本的 compose/Caddyfile/脚本

# 1) 拉起绿色实例（新版本），与旧单实例暂时并存（都健康、都在内网）
docker compose --profile green up -d sub2api-green
docker compose --profile green ps sub2api-green      # 等其 healthy

# 2) 让 caddy 加载新的蓝绿双 upstream
#    （旧配置指向 sub2api:8080，新配置指向 sub2api-blue/green:8080）
#    Caddyfile 是 bind mount，git pull 后文件已是新内容，热加载即可，无需重建容器：
docker compose exec caddy caddy reload --config /etc/caddy/Caddyfile
#    热加载不断开连接、零中断。确认 https://<域名>/health 正常后继续。
#    不要为发布重建 Caddy；共享 Caddy 只需 reload。

# 3) 记录活跃色为 green
echo green > .active_color

# 4) 停掉并移除旧单实例孤儿容器（它已不被 Caddy 路由，但仍挂着数据卷）
docker stop longdao-sub2api && docker rm longdao-sub2api

# 之后日常发布：改 .env 的 IMAGE_TAG → ./deploy.sh（只切换应用色）
```

> 迁移期 green 与旧单实例会短暂共用 `longdao_sub2api_data` 卷。业务真实状态在
> PostgreSQL，`/app/data` 主要是自动生成的 `config.yaml`，并存窗口短、风险可接受。
> 完成第 4 步后即恢复单实例占用。

## 如何填写 .env

所有密钥均在 compose 中用 `${VAR:?...}` 强制要求，缺失会导致 `docker compose config` 直接失败。

| 变量 | 必填 | 说明 |
|------|:---:|------|
| `IMAGE_REPO` | 否 | 镜像仓库前缀，默认 `ghcr.io/yaoruiquan/sub2api` |
| `IMAGE_TAG` | 是 | 版本化不可变 tag，禁止 `latest`，如 `2026.07.15-1` |
| `LONGDAO_DOMAIN` | 是 | 对外域名，Caddy 据此签发证书 |
| `LONGDAO_ACME_EMAIL` | 否 | Let's Encrypt 注册邮箱（证书到期通知） |
| `POSTGRES_PASSWORD` | 是 | 数据库密码 |
| `REDIS_PASSWORD` | 是 | **生产强制**;启用 Redis 认证 |
| `ADMIN_EMAIL` / `ADMIN_PASSWORD` | 是 | 首次自动创建的管理员 |
| `JWT_SECRET` | 是 | 固定值，否则重启后登录态失效 |
| `TOTP_ENCRYPTION_KEY` | 是 | 固定值，否则重启后 2FA 失效 |
| `TZ` | 否 | 时区，默认 `Asia/Shanghai` |

其余可选项（Gemini OAuth、URL 白名单、更新代理等）见 `.env.example` 注释。

## 起停与常用命令

```bash
# 查看当前活跃色
cat .active_color

# 查看状态与健康（两色都列出；平时只有活跃色在跑）
docker compose --profile blue --profile green ps

# 观察活跃色日志（把 <color> 换成 blue/green）
docker compose logs -f sub2api-<color> caddy

# 停止应用色（保留共享 PostgreSQL、Redis、Caddy）
docker compose --profile blue --profile green stop sub2api-blue sub2api-green

# 不要在此目录执行整栈 down 或 down -v；共享基础设施由独立运维流程管理。

# Caddy 配置变更应使用 reload，不要为发布强制重建 Caddy
docker compose exec -T caddy caddy validate --config /etc/caddy/Caddyfile
docker compose exec -T caddy caddy reload --config /etc/caddy/Caddyfile
```

## 发布与回滚（蓝绿零中断）

**日常发布**：改好 `.env` 的 `IMAGE_TAG`（新版本），然后一键：

```bash
./deploy.sh
#   内部顺序：检查 .env/镜像 → 备份 → 迁移(目标色) → switch.sh(起目标色→健康→冒烟→放量→停旧色) → 记录
#   KEEP_OLD=1 ./deploy.sh   # 切完保留旧色（便于快速回退），需之后手动停旧色
```

**只切换不走完整发布**（例如镜像已就位）：

```bash
./switch.sh                  # 切到另一色，用 .env 当前 IMAGE_TAG
OBSERVE_SECONDS=15 ./switch.sh
```

**回滚**（零中断，用旧 tag 部署到目标色再切）：

```bash
./rollback.sh 2026.07.16-1  # 回滚到指定历史版本 tag
#   若上次发布含不兼容迁移（删列/改类型），先 restore.sh 恢复数据库再回滚。
```

## GitHub Actions 自动部署

**华为云生产环境以 [HUAWEI.md](HUAWEI.md) 为准。** 当前服务器为 `121.37.128.126`，目录 `/www/longdao`，入口使用独立 Docker 和服务器发布器；下方旧蓝绿脚本不能直接在新机执行。已发布 workflow 由受限 SSH 兼容入口转入新发布器。

`.github/workflows/release.yml` 在 Release 成功后提供生产部署 job。为避免未配置凭据时误触发，
只有 production Environment 变量 `PRODUCTION_DEPLOY_ENABLED=true` 时才会执行。

在 GitHub 仓库的 `Settings -> Environments -> production` 中配置：

| 类型 | 名称 | 示例 |
|------|------|------|
| Variable | `PRODUCTION_DEPLOY_ENABLED` | `true` |
| Variable | `PRODUCTION_SSH_HOST` | `121.37.128.126` |
| Variable | `PRODUCTION_SSH_USER` | `root` |
| Variable | `PRODUCTION_DEPLOY_PATH` | `/www/longdao` |
| Variable | `PRODUCTION_URL` | `https://ai.longdaoec.com` |
| Secret | `PRODUCTION_SSH_PRIVATE_KEY` | 部署专用 SSH 私钥全文 |
| Secret | `PRODUCTION_SSH_KNOWN_HOSTS` | 经已认证管理连接核实的新机 SSH 主机公钥 |

建议为 Actions 单独创建部署密钥，只授权生产机的发布账号，并给 `production` Environment
配置 required reviewers。工作流会按版本串行执行：拉取不可变 GHCR 镜像 → 备份 → 迁移 →
蓝绿切换 → 外部 `/health` 检查；部署失败时保留旧活跃色。

## 数据库迁移

后端提供独立迁移命令 `sub2api -migrate`，与应用启动解耦：**先迁移成功，再放量新版本**。
`deploy.sh` 已在切换前自动调用 `migrate.sh`。手动执行（用一次性容器，`--rm` 不残留）：

```bash
# 对目标色（即将上线的非活跃色）执行迁移；MIGRATE_SVC 可覆盖
./migrate.sh
# 或直接指定某色：
# docker compose --profile green run --no-deps --rm sub2api-green /app/sub2api -migrate
```

迁移返回非零退出码即失败,此时不要放量新版本,排查后重跑。

## 网络边界说明（重要）

生产服务器的 Caddyfile 由龙道和 SEP 共同维护。更新龙道反代时只能修改龙道站点块，
必须保留 `longdaoSEP.cn` 站点；修改前备份，修改后执行 `caddy validate` 和 `caddy reload`。
不要用龙道项目本地 Caddyfile 整文件覆盖服务器配置。

```
       公网
        │  仅 80 / 443
        ▼
   ┌─────────┐   longdao-network (bridge, 内部)
   │  caddy  │──────────────────────────────────────┐
   └─────────┘  reverse_proxy 双 upstream              │
        │  sub2api-blue:8080 / sub2api-green:8080     │
        │  (主动健康检查 + lb_policy first)           │
        ▼         ▼                                  │
   ┌────────────┐ ┌────────────┐  ┌──────────┐  ┌───────────┐
   │sub2api-blue│ │sub2api-green│  │ postgres │  │   redis   │
   └────────────┘ └────────────┘  └──────────┘  └───────────┘
     （平时只跑活跃色一个；发布切换时短暂并存）
   （均无对外端口）                （无对外端口）   （无对外端口）
```

- **唯一对公网开放的是 caddy 的 80/443。** 两色应用、`postgres`、`redis` 都不映射宿主机端口。
- 应用只能经 Caddy 访问；Caddy 通过主动健康检查只把流量发送到健康的蓝或绿色实例。
- 需要临时调试内网服务时,可在对应服务临时加 `ports: ["127.0.0.1:<port>:<port>"]`
  （仅绑定回环,调试完移除),相关注释已写在 compose 文件里。

## 可信代理配置（务必阅读）

应用位于 Caddy 之后,真实客户端 IP 通过 `X-Forwarded-For` / `X-Real-IP` /
`CF-Connecting-IP` 透传。**如果后端不把 Caddy 容器网段列入可信代理,恶意客户端可以伪造
这些头绕过限流与 IP 风控。**

因此上线前请在应用配置(`config.yaml` 或对应环境变量)中,将 **可信代理范围设为
`longdao-network` 的容器网段**(bridge 网络默认 `172.x.0.0/16` 一类的私有段,可用
`docker network inspect longdao-network` 查看实际子网)。仅信任该网段,才应采信转发头里的
客户端 IP。本清单不修改后端代码,此项需在应用侧确认。

若前置了 Cloudflare,则真实 IP 以 `CF-Connecting-IP` 为准,同时应仅信任 Cloudflare 回源 IP 段。

## 流式响应(SSE)说明

Caddy 反代已设置 `flush_interval -1`,逐块立即刷新、不缓冲,保证 SSE / 逐 token 流式
响应实时返回;`transport http` 的读写超时给到 600s 以承载长连接。这是 DEP-002 的验收项。

## 验证清单

- [ ] `docker compose config -q` 无输出且退出码 0(必填项齐全、语法正确)。
- [ ] PostgreSQL、Redis、Caddy 和当前活跃应用均正常（发布期间短暂有两色应用）。
- [ ] `https://<域名>/health` 返回 200,且证书有效(浏览器无告警)。
- [ ] `http://<域名>` 自动 301 跳到 https。
- [ ] 调用一个流式接口,能看到 token 逐步返回而非一次性到达。
- [ ] 后端已配置可信代理网段(见上一节)。

## 运维脚本接口约定(供发布/备份脚本对齐)

| 约定项 | 值 |
|--------|-----|
| Compose 文件路径 | `deploy/production/docker-compose.yml` |
| 环境文件路径 | `deploy/production/.env`(真实值,不进仓库) |
| 应用服务名（蓝绿） | `sub2api-blue` / `sub2api-green`（compose profile 同名 blue/green） |
| 活跃色状态文件 | `deploy/production/.active_color`（运行时产物，`_active.sh` 维护） |
| 数据库服务名 / 卷 | `postgres` / `longdao_postgres_data` |
| 缓存服务名 / 卷 | `redis` / `longdao_redis_data` |
| 反代服务名 / 卷 | `caddy` / `longdao_caddy_data`、`longdao_caddy_config`、`longdao_caddy_logs` |
| 应用数据卷（两色共用） | `longdao_sub2api_data` |
| 内部网络名 | `longdao-network` |
| 镜像变量 | `IMAGE_REPO`(默认 `longdao/sub2api`)+ `IMAGE_TAG`(必填,禁止 latest) |
| 迁移调用 | `./migrate.sh`（对目标色一次性 `--no-deps` 容器；先迁移成功再放量） |
| 优雅关闭 | 应用 `SHUTDOWN_TIMEOUT_SECONDS=60` + 容器 `stop_grace_period=90s` |
| 对外端口 | 仅 caddy 的 `80` / `443` |

标准发布：改 `.env` 的 `IMAGE_TAG` → `./deploy.sh`（内部：备份 → 迁移 → 蓝绿切换）。
回滚：`./rollback.sh <旧版本tag>`（零中断，用旧 tag 部署到目标色再切）。切勿依赖 latest。
