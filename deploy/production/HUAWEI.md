# 华为云生产发布

当前正式服务器为 `121.37.128.126`，唯一正式域名为 `ai.longdaoec.com`。旧域名 `longdaoai.cn` 已从运行链路移除，不再作为兼容入口。不要在新机运行本目录旧的 `deploy.sh` / `switch.sh` / `docker-compose.yml`；这些属于原服务器部署结构。

2026-11-07 已完成运行时域名切换：SEP 的 `SUB2API_BASE_URL` 与 `MEDIA_VIDEO_BASE_URL`、龙道 OAuth 回调、支付宝/易支付回调均使用 `ai.longdaoec.com`；新机 Caddy 不再信任旧服务器 IP。历史订单中的旧地址保留作审计记录，不作为当前配置。

## 服务器入口

发布代码安装于 `/www/longdao/deploy/huawei_release.py`，仅依赖 Python 标准库；配置和私密内容均在 `/www/longdao/deploy`。此入口固定使用 `unix:///run/longdao-docker.sock`，只用主 Docker socket 校验/热加载现有 `sep-caddy`，不管理 SEP 容器生命周期。

```sh
# 管理员在新服务器执行，只检查，不发布
python3 /www/longdao/deploy/huawei_release.py --check

# 发布已构建的不可变版本
python3 /www/longdao/deploy/huawei_release.py --version 2026.10.21-2

# 查看本项目
/www/longdao/deploy/compose ps --all
cat /www/longdao/deploy/last-release.json
```

`--redeploy` 是管理员同版本演练选项；CI 不开放这个参数。普通同镜像重跑只验证镜像 ID、实际版本、原 Key 的模型目录和新域名，不重启当前实例。

## 发布过程

1. 独占发布锁；核实本机 machine-id、数据盘 UUID、所有持久目录、独立 Docker Root、PG/Redis/当前应用健康、SEP 连续运行状态，以及 Caddy AI upstream。
2. 从正式 GHCR 仓库拉取指定版本至独立 Docker，验证 linux/amd64。
3. 备份 PostgreSQL 自定义格式归档、应用文件、Redis RDB、私密部署配置和 Caddy 配置；生成 SHA256 清单。备份位于 `/www/longdao/backups/release-<UTC>-v<version>`，权限限制为 root。应用正在追加的 `app-data/logs` 不进发布快照，原日志继续保留在数据盘。
4. 用目标镜像执行独立 `-migrate` 命令；失败不启动目标应用。迁移必须兼容当前版本，脚本不自动恢复数据库。
5. 启动目标色：green 使用 `172.19.0.1:18080`，blue 使用 `172.19.0.1:18081`。两色共享原应用数据、业务库及 Redis，环境密钥不重生成。
6. 验证目标容器健康、镜像 ID、公开版本、页面、原 Key 的模型目录；不发起付费模型请求。
7. 保留其他 Caddy 配置，仅替换 `LONGDAO-AI` 标记块中的 upstream，校验后原位写入并 reload。验证新域名及 SEP 连续运行。
8. 保存与入口一致的 Compose，关闭旧色自动重启并优雅停止旧色，写入 `last-release.json`。

首次发布后 Compose 存在两色服务，但日常只能显式启动一个服务。不要使用不带服务名的 `compose up`；不要在主 Docker 运行本项目。

发布快照属于在线备份，各存储的快照时间有先后，不承诺 PG、Redis 与文件之间的跨存储原子一致性。旧迁移冻结备份另行保留；发布备份不自动删除，不代替异地定时备份。

## Seedance 旧版视频适配的部署配置

应用代码保留 `/v1/videos`，账号凭据 `video_protocol=naoshengyun_legacy` 才启用旧协议。已有账号缺省为 `openai_videos`。以下配置对应开发方案，不代表服务器已经完成配置。

- 通过当前 `/www/longdao/deploy/huawei_release.py` 管理的配置，为 blue、green 添加相同挂载：`/www/longdao/media/video-inputs:/app/media/video-inputs`。不要使用本目录旧 Compose 发布。
- 检查真实应用 UID/GID，图片根目录权限为 `0700`，图片及清单为 `0600`。该目录不放入公开静态目录，不进入每次发布的全量应用文件复制。
- 在应用私密配置中填写 `video_legacy`，字段见 `deploy/config.example.yaml`。配置实际容量、剩余空间阈值、并发、已确认的模型规格和精确 CDN 域名；未配置的规格和下载域名会被拒绝。
- `signing_keys` 使用独立的至少 32 随机字节的 base64 密钥；两色一致、重启不重生成。轮换后保留旧密钥至旧链接全部到期。不得把实际密钥提交仓库。
- Caddy 将 `/media/video-inputs/` 代理给应用，不使用 `file_server`，不加 API Key 或浏览器登录校验，不缓存响应；访问日志隐藏签名查询串。应用普通日志只使用资源编号，不记录完整签名 URL。
- 核对 Caddy 的传输期限和连接关闭行为，匹配应用下载总预算（示例 150 秒）及 SEP 的 180 秒限制；文件 GET 不包含账号密钥，Longdao 不保存成品视频。
- 非付费检查包括外网 GET/HEAD 读取签名参考图、两色挂载及密钥一致、重启后可读、到期拒绝和清理。任务、图片、幂等记录继续使用原 Redis，期限固定 7 天。SEP 原 24 小时成品缓存不变。
## GitHub production Environment

| 类型 | 名称 | 华为云值 |
|---|---|---|
| Variable | `PRODUCTION_DEPLOY_ENABLED` | `true`（完成安装与验收后启用） |
| Variable | `PRODUCTION_SSH_HOST` | `121.37.128.126` |
| Variable | `PRODUCTION_SSH_USER` | `root` |
| Variable | `PRODUCTION_DEPLOY_PATH` | `/www/longdao` |
| Variable | `PRODUCTION_URL` | `https://ai.longdaoec.com` |
| Secret | `PRODUCTION_SSH_PRIVATE_KEY` | 新机专用 Actions 密钥，非旧机密钥 |
| Secret | `PRODUCTION_SSH_KNOWN_HOSTS` | 从已认证的新机管理会话取得的主机公钥 |

SSH 公钥安装在新机 `authorized_keys`，带 `restrict,command="/www/longdao/deploy/huawei_ci_entry.py"`。只能运行：

```text
longdao-preflight
longdao-deploy 2026.10.21-2
```

兼容入口仍严格接受旧版已发布 workflow 的 `DEPLOY_PATH=/www/longdao VERSION=<version> bash -s` 格式：只提取固定路径和合法版本，消费并丢弃旧 heredoc，转入新服务器发布器，绝不执行旧栈脚本。因此既有标签和在线旧 workflow 也能在新机正确发布；无需修改已有 Git 标签或让服务器保留旧仓库结构。

仓库新版 `release.yml` 已改成直接调用 `longdao-deploy`，本地代码未提交/推送时，线上仍通过上述兼容入口工作。未来发布同样使用 GitHub production Environment，已发布 workflow 与新版 workflow 都能进入相同的新机发布器。普通 main 推送不触发 Release；版本标签 `v*` 或手动 Release 才触发。

## 失败处理

日志：`/www/longdao/logs/release-<UTC>.log`，含迁移等私密诊断，不上传公开日志。CI 只输出阶段与失败位置，不输出密钥或模型目录内容。

- 迁移、目标启动或切换验证失败：保留/恢复旧 upstream，停止失败目标色；数据库不自动回滚。
- Caddy 被其他进程同时修改：拒绝覆盖；需人工检查现有入口，保留诊断文件。
- 新色已验收并提交配置后，若旧色排空失败或退出码非 0：CI 返回失败，**保持新入口，不自动切回可能已经停止的旧色**。旧色非正常退出会记录 `active-drain-error`。检查新色健康、`compose.json` 与 Caddy 实际 upstream 后，单独处理旧色。现有应用关闭窗口为 60 秒，超长流式请求可能超过该窗口；不承诺任意时长请求都能无中断排空。
- 不要恢复旧服务器的冻结数据库来“回滚版本”；新站已承接业务写入。
- 只做向后兼容迁移才能让旧应用继续服务；删列等不兼容迁移必须另行安排维护，脚本不能消除这种风险。

密钥轮换时先安装新机受限公钥并验签，再替换 GitHub Secret。不要在文档、日志或 Git 中存储实际私钥、JWT、TOTP、数据库密码或 smoke Key。
