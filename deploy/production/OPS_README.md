# 龙道 AI 生产运维手册

**华为云当前生产环境以 [HUAWEI.md](HUAWEI.md) 为准。** 以下默认 `.env`、Compose 和旧目录的命令属于原部署结构，不直接适用于 `/www/longdao` 的独立运行时。

本目录存放龙道 AI Token 中转平台的生产运维脚本，配合生产编排文件
（`docker-compose.yml` / `.env` / `Caddyfile`）使用。所有脚本均为 `set -euo pipefail`
的健壮 bash，凭据一律从 `.env` 读取，脚本内不含任何真实密钥。

## 目录约定

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `COMPOSE_FILE` | `deploy/production/docker-compose.yml` | 生产 compose 文件 |
| `ENV_FILE` | `deploy/production/.env` | 生产环境变量（真实值，不进仓库） |
| `BACKUP_DIR` | `deploy/production/backups` | 备份产物目录 |
| `DEPLOY_LOG` | `deploy/production/deploy.log` | 发布/回滚审计日志 |
| `IMAGE_REPO` | `ghcr.io/yaoruiquan/sub2api` | 镜像仓库前缀 |

所有脚本都通过 `docker compose -f "$COMPOSE_FILE" --env-file "$ENV_FILE" ...` 调用，
路径可用上述环境变量覆盖，便于在不同机器/目录复用。

## 脚本一览

| 脚本 | 用途（一句话） |
|------|----------------|
| `migrate.sh` | 对生产 stack 执行独立数据库迁移（`sub2api -migrate`），失败返回非零。 |
| `backup.sh` | 备份 PostgreSQL + `/app/data`，命名含环境/版本/时间戳，支持保留最近 N 份。 |
| `restore.sh` | 从备份恢复，默认恢复到测试库，带强制确认，生产库需二次确认。 |
| `deploy.sh` | 标准发布流程编排：检查→备份→迁移→up→健康检查→冒烟→记录日志。 |
| `rollback.sh` | 切回上一镜像版本并健康检查，绝不触碰数据卷。 |
| `smoke-test.sh` | 发布后冒烟测试，P0（`/health`）失败即非零退出。 |

## 前置准备

1. 构建不可变版本镜像（在仓库根执行）：
   ```bash
   ./deploy/build_production_image.sh 2026.07.15-1
   ```
2. 准备 `deploy/production/.env`（由 `.env.example` 复制并填真实值），
   至少包含：`IMAGE_TAG`、`POSTGRES_PASSWORD`、`REDIS_PASSWORD`、`JWT_SECRET`、
   `TOTP_ENCRYPTION_KEY` 等。`IMAGE_TAG` 必须显式指向不可变 tag，切勿用 `latest`。

## 标准发布流程（deploy.sh，蓝绿零中断）

编排顺序，任一步失败即停止并提示回滚：

1. 检查 `.env` 存在、`IMAGE_TAG` 已设置。
2. 检查目标镜像存在（`docker image inspect`），不存在提示先构建。
3. **发布前备份**（调用 `backup.sh`）。
4. **执行迁移**（调用 `migrate.sh`，对目标色一次性容器）；失败则中止，**不放量**。
5. **蓝绿切换**（调用 `switch.sh`）：拉起目标色 → 等健康 → 容器内冒烟 →
   Caddy 主动健康检查放量 → 停旧色（SIGTERM，应用 60s 优雅 drain 在途流式请求）。
   全程至少一个健康实例在线，**零中断**。
6. 记录结果到 `deploy.log`。

```bash
# 发布（先在 .env 里设置好目标 IMAGE_TAG）
./deploy/production/deploy.sh
# 切完保留旧色以便快速回退（之后需手动 stop 旧色）：
# KEEP_OLD=1 ./deploy/production/deploy.sh
```

失败时切换脚本会自动回滚（停掉未就绪/未通过冒烟的目标色，现网继续由旧色提供服务）。

## 回滚流程（rollback.sh，蓝绿零中断）

```bash
# 回滚到上一个不可变版本
./deploy/production/rollback.sh 2026.07.16-1
```

- 用旧 `IMAGE_TAG` 走蓝绿切换：把旧版本部署到**目标色**并放量、再停当前色，
  **不修改 `.env`**，**绝不删除或重建 postgres/redis 数据卷**，全程零中断。
- 切换中自动健康检查 + 冒烟；不通过则保持现网不变并报错。
- **迁移与回滚关系**：迁移默认「前向修复」，回滚只切应用镜像，不回退数据库结构。
  若上次发布含**不兼容迁移**（删列/改类型等），仅切镜像可能失败，
  需先用 `restore.sh` 从发布前备份恢复数据库，再切镜像。
- 若要长期停留在旧版本，回滚后把 `.env` 的 `IMAGE_TAG` 也改成该版本。

## 备份与恢复

### 备份（backup.sh）

```bash
./deploy/production/backup.sh
KEEP=30 ./deploy/production/backup.sh        # 保留最近 30 份
```

- 产物：`pg_<env>_v<tag>_<时间戳>.sql.gz` 与 `data_<env>_v<tag>_<时间戳>.tar.gz`。
- 保留策略：默认保留最近 `KEEP=14` 份，更老的自动清理。
- **异地复制（重要）**：至少一份备份必须复制到服务器之外（对象存储/另一磁盘/另一台机器），
  防止单机损坏导致备份与数据同时丢失。两种方式：
  - 在 `backup.sh` 底部取消注释 `rsync` / `aws s3 cp` 示例；
  - 或设置 `OFFSITE_HOOK=/path/to/hook.sh`，脚本会对每个产物调用该 hook 完成上传。

### 恢复演练（restore.sh）

```bash
# 默认恢复到测试库 sub2api_restore_test（安全，不覆盖生产）
./deploy/production/restore.sh deploy/production/backups/pg_prod_v2026.07.15-1_xx.sql.gz

# 恢复到生产库（高危，需二次确认输入库名）
./deploy/production/restore.sh backups/pg_xxx.sql.gz sub2api
```

- 默认恢复到测试库并先 DROP/CREATE 保证干净；恢复到生产库不自动 DROP，需人工评估。
- 交互式强制确认；非交互场景需 `FORCE=1`（谨慎）。
- 恢复后按提示核对 `users` / `api_keys` 等表行数是否合理。
- 建议**每月至少做一次恢复演练**，验证备份真实可用（OPS-001）。

## 冒烟测试（smoke-test.sh）

```bash
# 容器内直连
./deploy/production/smoke-test.sh
# 经 caddy 外部地址
BASE_URL=https://api.example.com ./deploy/production/smoke-test.sh
```

- 已自动化：`/health`（P0，200 必测）、首页可访问（P1）、注册入口策略探测（P1）。
- 输出含版本、时间、环境信息。P0 失败即非零退出，`deploy.sh` 据此终止放量。
- **需凭据、暂未自动化的项**（脚本内以 TODO 标注，勿在脚本硬编码任何凭据）：
  管理员登录 + TOTP、创建用户/生成 API Key、用 API Key 发真实模型调用、
  订阅额度扣减与风控事件核对。可在受控环境注入临时凭据后扩展脚本。

## cron 每日备份建议

在生产主机 crontab（`crontab -e`）中加入（示例，按实际路径调整）：

```cron
# 每日 03:00 备份，日志写入文件，异地复制通过 OFFSITE_HOOK 完成
0 3 * * * cd /opt/longdao/sub2api && OFFSITE_HOOK=/opt/longdao/offsite-upload.sh \
  ./deploy/production/backup.sh >> deploy/production/backups/cron.log 2>&1
```

- 备份脚本失败会返回非零退出码，建议接入监控/告警（如 healthchecks.io、邮件、企业微信）。
- 定期检查备份产物大小与异地副本是否同步。

## 发布前检查清单

- [ ] 已用 `build_production_image.sh` 构建目标不可变版本镜像（基于已提交 commit）。
- [ ] `.env` 已配置且 `IMAGE_TAG` 指向该版本（非 `latest`）。
- [ ] 关键密钥（`POSTGRES_PASSWORD`/`REDIS_PASSWORD`/`JWT_SECRET`/`TOTP_ENCRYPTION_KEY`）已设置。
- [ ] 最近一次备份成功且已异地复制。
- [ ] 已知晓本次是否包含不兼容迁移；如是，回滚需配合恢复备份。
- [ ] 记录好上一个可回滚的 `IMAGE_TAG`。

## 相关规范

呼应生产上线规范 spec §10（发布/回滚）、§13.1（备份与恢复），
以及 OPS-001（备份/恢复）、OPS-002（回滚）、MIG（迁移）、TST-001（冒烟）条目。
