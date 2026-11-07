# 迁移后部署配置核查

## 后续处理：已完成自动部署迁移

用户随后授权迁移自动部署，已于北京时间 2026-11-03 17:50 完成 GitHub 实际部署验收。production Environment 已改为 `121.37.128.126`、`/www/longdao`、`https://ai.longdaoec.com`，新机专用受限密钥及 known_hosts 已更新，自动部署已启用。服务器发布器已安装，同版本完整切换演练和 GitHub 部署 job 均通过，当前活跃色为 blue（18081），green 已停止。

下方是修复前核查的历史记录，不能据此判断当前自动部署仍指向旧机。最终结果见 [自动部署迁移交付](relay-autodeploy-migration-20261103.md)。

---

日期：2026-11-03。范围：本地部署代码、GitHub 正式仓库的 production Environment 与在线 workflow、新旧服务器当前部署结构。仅核查及更新本地记录，未触发发布、修改 GitHub 变量/Secrets、提交或推送代码。

## 结论

**业务运行已在新机，但自动部署尚未迁移。当前不能认为下一次发布会部署到新机。**

这是前次迁移遗漏的持续部署配置；之前交付的线上恢复及数据核对结论有效，不能据此推断 CI/CD 也已迁移完成。

## 已核实正确的线上配置

| 配置 | 实际值 |
|---|---|
| 当前应用服务器 | `121.37.128.126` |
| 当前业务数据库 | 新机独立 PostgreSQL，`127.0.0.1:15432`，数据库 `sub2api` |
| 当前 Redis | 新机独立 Redis，`127.0.0.1:16379` |
| 应用入口 | 新机 `172.19.0.1:18080`，由既有 `sep-caddy` 代理 |
| 业务地址设置 | `api_base_url`、`frontend_url`、`balance_low_notify_recharge_url` 均为 `https://ai.longdaoec.com` |
| 独立 Docker | `/run/longdao-docker.sock`；持久目录 `/www/longdao/runtime/docker` |
| 部署文件 | `/www/longdao/deploy/compose.json`，通过同目录 `compose` 包装器管理 |
| 存储 | `/dev/sdb1` 挂载 `/www` |
| 在线检查 | 新旧域名 `/health` 均 HTTP 200 |

旧域名继续由旧机转发属于已确认兼容策略；OAuth/支付回调保留旧地址属于有意保留，不能把所有 `longdaoai.cn` 一概替换。上游按用户要求不在本轮重新排查。

## 尚未迁移的 GitHub 自动部署

正式仓库：`yaoruiquan/LongdaoAI`，Environment：`production`。以下来自 GitHub 在线查询，不只是仓库示例：

| 变量 | 当前值 | 判断 |
|---|---|---|
| `PRODUCTION_DEPLOY_ENABLED` | `true` | 仍启用旧自动部署 |
| `PRODUCTION_SSH_HOST` | `64.83.39.223` | 仍指向旧机，应迁移到 `121.37.128.126` |
| `PRODUCTION_SSH_USER` | `root` | 原账号；新机也支持 root，但需配套新机部署密钥 |
| `PRODUCTION_DEPLOY_PATH` | `/opt/longdao` | 仍为旧机目录，不能直接用于新机 |
| `PRODUCTION_URL` | `https://longdaoai.cn` | 旧兼容入口，不能证明新版本已在新机生效 |

`PRODUCTION_SSH_PRIVATE_KEY` 和 `PRODUCTION_SSH_KNOWN_HOSTS` 均存在，最后更新时间为 2026-09-10；未读取 Secret 值，不能据此声称已验证对新机可用。切换时需配置新机可用的专用密钥和已核实的新主机 host key。

在线 Release workflow 与本地逻辑一致：版本标签 `v*` 推送或手动运行 Release 触发构建，成功后进入 production 部署。普通 main 推送本身不会触发该 Release workflow。最近一次 `v2026.10.21-2` 的 `Deploy production` job 成功，说明该链路不是从未使用的模板。

工作流当前实际执行：SSH 到上述旧 IP → `cd /opt/longdao` → `git fetch origin main` → 修改旧 `deploy/production/.env` 的镜像版本 → 使用默认 Docker 拉取镜像 → 执行旧 `deploy/production/deploy.sh`。

因此再次触发 Release 仍会尝试在旧机执行发布；不能因为旧应用的 restart policy 改为 `no` 就认为显式部署命令也被禁用。未实际触发部署，也不预测旧脚本在当前转发配置下能否完整成功。

## 不能只修改 IP 的原因

1. 新机 `/www/longdao` 没有旧流程要求的 Git checkout、`deploy/production/deploy.sh`、`deploy/production/.env`。直接改 host/path 会在结构检查或 `git fetch` 阶段失败。
2. 旧流程使用默认 `docker`，新机默认 daemon 管理 SEP。本项目必须使用 `/run/longdao-docker.sock`，否则镜像会落入 SEP 原运行时，违反本项目镜像和容器层落数据盘的要求。
3. 旧蓝绿脚本要求 blue/green 两个服务、`.active_color` 和 `sub2api-blue:8080` / `sub2api-green:8080` 双 upstream。新机当前仅有 `sub2api-green`，使用 host 网络及 `172.19.0.1:18080` 单 upstream，不能原样执行。
4. 旧脚本在同一 daemon 查找 `longdao-caddy`。新机入口是另一个 daemon 的 `sep-caddy`；发布必须只更新 AI 站点块并热加载，不能套用旧栈去创建 Caddy 或重建 SEP 服务。
5. 原发布前备份脚本不完整覆盖 Redis 和私密部署配置，需适配新目录与恢复方式；现有冻结快照仍保留有效。

## 需要补齐的变更

1. 在启用新链路前阻断旧机自动发布，避免继续操作冻结旧账本。
2. 编写并安装适配数据盘、独立 Docker、独立 PG/Redis、共享 Caddy 的新发布入口，校验目标机和挂载点；准备发布前完整备份、健康检查和失败处理。
3. 如保留蓝绿发布，为两色设置不同监听端口，明确切换 Caddy AI upstream 的方式及活跃状态记录；不可直接启动两个争抢 18080 的实例。
4. 修改 workflow，使用新入口和独立 Docker；同步部署文档、示例路径及新域名验收地址。
5. 更新 production Environment 的 host/path/URL、SSH 密钥及 known_hosts，完成无业务变更的联通/配置检查后，再做一次受控发布验收。
6. 发布后校验实际容器镜像版本及新机响应，不能仅看旧域名 `/health` 200 就宣布发布成功。

以上变更尚未执行，本次未更改远端自动部署开关或其他生产设置。

## 证据位置

- [.github/workflows/release.yml](../.github/workflows/release.yml:317)：production job。
- [旧机路径默认值](../.github/workflows/release.yml:361)、[旧发布步骤](../.github/workflows/release.yml:382)、[旧验收地址](../.github/workflows/release.yml:405)。
- [共享辅助脚本](../deploy/production/_active.sh:69)：默认 Docker 与旧 Compose 调用方式。
- [双 upstream 检查](../deploy/production/switch.sh:168)：与新机单端口结构不兼容。
- [仓库部署文档](../deploy/production/README.md:174)：示例仍为旧地址。
- GitHub 在线 production Environment 变量、Secret 名称及更新时间、Release workflow、最近一次发布 job 状态，已通过 `gh` 只读查询。
