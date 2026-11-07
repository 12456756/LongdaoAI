# 龙道 AI 迁移交付记录

2026-11-03，北京时间。已从 `64.83.39.223` 迁移至华为云 `121.37.128.126`。

## 访问与数据

- 新入口：`https://ai.longdaoec.com`，HTTPS、首页、健康检查及公开设置接口验证通过。
- 原入口：`https://longdaoai.cn`，由旧机 Caddy 转发新站，可继续使用原地址。
- 保留 22 个用户、74 个 API Key、47 个上游账户、271 条设置、28 条支付订单、220,914 条使用日志；余额及密钥均包含在完整数据库核对中。
- 80 张表的记录数及全部内容指纹、65 个序列状态，在新应用启动前与冻结旧库完全一致。
- 原 Key 在两个域名调用 `/v1/models` 均返回 200、7 个模型，响应指纹与迁移前一致；未发起付费模型请求。未认证访问返回 401。
- 保持应用版本 `2026.10.21-2`，JWT、TOTP 和业务密钥保留。
- 仅将站点 API 地址、前端地址、余额不足充值链接改为新域名；OAuth、支付回调及上游地址保留。

维护时间：**16:37:19—17:03:04，约 25 分 45 秒**。新域名于 17:02:36 验证恢复。维护期间，依赖中转的 SEP 模型功能受影响，SEP 站点持续运行。

## 数据盘与运行时

所有项目持久目录均在 1 TB 数据盘 `/www`，逐项 `findmnt -T` 验证设备为 `/dev/sdb1`：应用文件、数据库、Redis、备份、镜像、容器层和日志。证书与入口访问日志复用 `/www/sep/caddy`，同样位于数据盘。

本项目使用独立 Docker/containerd，未移动或重启 SEP 原运行时。SEP 六个原有容器均运行、重启次数为 0；核心容器启动时间未变。Caddy 仅热加载新增站点。`sep.longdaoec.com`、`longdaosep.cn` 首页均返回 200。

SEP 原 `SUB2API_BASE_URL=https://longdaoai.cn/v1` 保留，旧域名的原 Key 验证通过。未发起 SEP 付费生成请求。

## 运维入口

普通 `docker ps` 显示 SEP 运行时。管理本项目使用专用入口：

```sh
# 在新服务器执行
/www/longdao/deploy/compose ps
/www/longdao/deploy/compose logs --tail=100 sub2api-green
docker -H unix:///run/longdao-docker.sock ps
systemctl status longdao-containerd longdao-docker --no-pager
findmnt -T /www/longdao/postgres
```

| 组件 | 名称 / 位置 |
|---|---|
| 部署配置 | `/www/longdao/deploy/compose.json`，通过同目录 `compose` 包装器执行 |
| 应用 | `longdao-ai-green`，`172.19.0.1:18080`，数据 `/www/longdao/app-data` |
| PostgreSQL | `longdao-ai-postgres`，`127.0.0.1:15432`，数据 `/www/longdao/postgres` |
| Redis | `longdao-ai-redis`，`127.0.0.1:16379`，数据 `/www/longdao/redis` |
| 独立 Docker | `/run/longdao-docker.sock`；持久目录 `/www/longdao/runtime/docker` |
| 独立 containerd | `/run/longdao-containerd/containerd.sock`；持久目录 `/www/longdao/runtime/containerd` |
| Caddy 配置 | `/www/sep/deploy/Caddyfile`，AI 站点带 `LONGDAO-AI` 标记 |
| 入口日志 | `/www/sep/caddy/logs/longdao-ai.log` |
| 运行时日志 | `/www/longdao/logs` |

独立 Docker 使用 host 网络并禁用默认 bridge/iptables 管理。PG、Redis 只监听回环地址；应用只监听 SEP Docker 网桥网关，由现有 Caddy 访问，未增加公网监听。Caddy 保留客户端地址转发，应用信任对应代理。

两个 systemd 服务均启用开机启动并要求 `/www` 已挂载，容器采用 `unless-stopped` 重启策略。切换前独立 Docker 重启恢复演练通过；未进行生产整机重启。

上线验收：应用、PG、Redis 均 healthy、重启次数 0、无 OOM，启动日志未发现 fatal/panic。Redis 已从最终 RDB 恢复并启用 AOF，重写和写入状态正常；过期键按原绝对过期时间自然失效。

## 备份与验证证据

- 旧机冻结备份：`/opt/longdao/migrations/20261103/final`。
- 新机最终备份及验证证据：`/www/longdao/backups/20261103-final`。
- 本地私密备份：`/Users/yao/.codex/backups/longdao/20261103-migration`，包含最终原始备份和新机部署配置，文件权限限制为当前用户。
- PostgreSQL 备份为自定义格式；导入空库时使用 `--exit-on-error --single-transaction --no-owner`，约 28.31 秒完成。
- `manifest.json` 与 `target-restored-manifest.json` 保存全部表与序列的核对结果；校验工具见 [database-manifest.py](migration-20261103/database-manifest.py)。
- `checksums.json` 保存原始备份文件大小和 SHA256；跨机传输后已核对。
- `site-url-changes.json` 记录仅三项站点设置变更；`target-validation.json` 记录健康状态和 SEP 连续运行证据；`post-cutover-smoke.json` 位于旧机最终备份中，记录原 Key 验证。
- 运行后的业务写入会改变数据，不再要求运行中数据库与冻结清单完全相同。

冻结数据库清单 SHA256：

```text
c58a8bc8ccb2ea0daab53d917e0a78fe5ec02e35f7900b6cf50d540330e8002f
```

镜像归档 SHA256：

```text
34dba0470f92820d35ee4fa0acf3878d5b2607469ecd989dec4d30102a2f6684
```

新旧运行时镜像存储方式不同，显示 ID 有差异；已核对应用、PG、Redis 的架构、操作系统、Config 及 RootFS，一致。

## 保留事项及回退限制

**自动部署后续已完成迁移：** GitHub production Environment 已改为新机 `121.37.128.126`、`/www/longdao` 和新域名，配套受限 SSH 入口与独立 Docker 发布器已安装。北京时间 17:50 实际 GitHub 部署 job 成功；同版本完整切换演练也通过，当前活跃色为 blue（18081）。详见 [自动部署交付](relay-autodeploy-migration-20261103.md)。修复前问题保留于 [历史核查](relay-deployment-config-audit-20261103.md)。

按用户要求，上游原样迁移并暂缓排查；没有进行付费模型、真实支付、邮件、OAuth 外部登录或后台人工登录验收，不将这些路径记为已通过。

本次保留迁移快照和配置归档，未新增长期定时备份任务。后续备份应覆盖数据库、应用文件、Redis 和私密部署配置，并写入数据盘。

旧机 green/blue 应用保持停止、restart policy 为 `no`，避免双写。旧 Caddy、共享 PG、Redis 及其他服务未停止。**旧域名仍需旧机在线，当前不能释放旧服务器。**

新站已承接写入，禁止直接启动旧应用回滚至冻结旧库。若需回迁，应先冻结新站，再同步最新业务数据；旧机 Redis 供其他服务共用，禁止整份覆盖。

完整方案见 [迁移方案与执行结果](longdao-huawei-migration-plan-20261103.md)，原现场调研见 [执行前归档](migration-20261103/pre-execution-research.md)。
