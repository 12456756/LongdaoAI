# 自动部署迁移交付

日期：2026-11-03，北京时间。状态：**已迁移并启用，GitHub 实际部署验证通过**。

## 以后发布到哪里

正式仓库 `yaoruiquan/LongdaoAI` 的 Release 流程继续使用。推送版本标签 `v*` 或手动运行 Release，在构建成功后部署到 **华为云 `121.37.128.126`**。普通 main 推送本身不触发 Release。

| GitHub production Environment | 当前值 |
|---|---|
| `PRODUCTION_DEPLOY_ENABLED` | `true` |
| `PRODUCTION_SSH_HOST` | `121.37.128.126` |
| `PRODUCTION_SSH_USER` | `root` |
| `PRODUCTION_DEPLOY_PATH` | `/www/longdao` |
| `PRODUCTION_URL` | `https://ai.longdaoec.com` |

`PRODUCTION_SSH_PRIVATE_KEY` 已换为新机专用密钥，公钥限制为项目发布入口，不能任意执行 SSH shell。`PRODUCTION_SSH_KNOWN_HOSTS` 已换为从新机已认证管理连接取得的主机公钥。两个 Secret 最后更新于北京时间 17:48:21—17:48:22。

## 实际执行与验收

- 已先暂停旧自动部署，完成新机入口配置和演练后重新启用；旧机应用仍停止，未被自动重启。
- 新机使用当前版本 `2026.10.21-2` 完整演练：发布前备份 → 独立数据库迁移 → blue 实例健康与版本检查 → 原 Key 目录验证 → Caddy AI upstream 切换 → 新旧域名验证 → 旧 green 排空停止。
- 完整演练备份：`/www/longdao/backups/release-20261103T094536150843Z-v2026.10.21-2`，文件大小和 SHA256 已核对。
- 当前运行 `longdao-ai-blue`，监听 `172.19.0.1:18081`，healthy、重启 0、无 OOM；`longdao-ai-green` 已停止，restart policy 为 `no`。PG/Redis 继续 healthy、重启 0。
- 已为 blue 的 18081 增加仅允许 SEP Caddy 来源的 UFW 规则；两个颜色端口都只绑定 Docker 网桥地址。
- GitHub 实际重跑既有 Release 的部署 job：run `39589207123`，attempt `2`，job `125843406544`，结果 **success**，北京时间 **17:49:54—17:50:06**。
- GitHub 日志确认 `DEPLOY_HOST: 121.37.128.126`、`DEPLOY_PATH: /www/longdao`、`PRODUCTION_URL: https://ai.longdaoec.com`，并输出 `SUCCESS: requested image already active on 121.37.128.126; version and both domains verified`。
- 该 GitHub 验收是当前版本的重复部署：实际镜像已一致时执行验证并跳过重启。完整容器切换在此前服务器演练中单独通过，未构建或升级新版本。
- 新旧中转域名和两个 SEP 域名首页均 HTTP 200；原 API Key 的模型目录在新旧中转域名一致。未进行付费模型调用。
- SEP 六个容器 StartedAt 与迁移前一致，重启次数均 0；本次仅热加载 Caddy AI upstream。
- 12 项发布边界及失败路径回归测试通过，Python 语法和 workflow YAML 解析通过，`git diff --check` 通过。

演练中发现实时日志写入导致 tar 返回“file changed”，当时在备份阶段安全中止、未切换；已把实时日志排除出发布快照，日志继续保留数据盘。还补充了从 Caddy 容器直接验证目标端口的步骤，入口可达后才允许切流。

演练旧 green 关闭时出现 `context deadline exceeded`，退出码为 1、无 OOM；新 blue 已接流并保持健康。因此本次证明切换主路径可用，不将旧色关停宣称为完全平滑排空。已补充非 0 退出码检查与回归测试：后续发生同类情况，发布返回失败并记录 `active-drain-error`，保留健康新入口，不回切到已经停止的旧色。当前 60 秒应用关闭窗口仍可能中断超过该窗口的旧在途请求。

## 安装文件与发布安全边界

服务器安装：

| 文件 | 用途 |
|---|---|
| `/www/longdao/deploy/huawei_ci_entry.py` | 受限 SSH 命令入口，仅提取合法版本并调用新发布器 |
| `/www/longdao/deploy/huawei_release.py` | 数据盘、独立 Docker、备份、迁移、切流与失败处理 |
| `/www/longdao/deploy/release-target.json` | 新机 machine-id 和数据盘 UUID 绑定，内容仅存服务器/私密备份 |
| `/www/longdao/deploy/smoke-key.private` | 原有效 Key，仅用于只读模型目录验证，不输出凭据 |
| `/www/longdao/deploy/last-release.json` | 最近发布版本、镜像 ID、活跃色及结果 |
| `/www/longdao/deploy/HUAWEI.md` | 当前运维说明 |

发布仅使用 `/run/longdao-docker.sock`，不会把镜像落到 SEP 的默认 Docker。旧项目的共享数据库、Redis 和旧 Caddy 结构不再参与新发布。两色端口为 green 18080 / blue 18081；旧色被关闭自动重启，Compose 两色均带 profile，避免默认同时拉起。

发布前备份包含 PG 自定义归档、应用恢复文件、Redis RDB、部署配置及 Caddy 配置。实时应用日志继续保留在原数据盘，不进发布快照。备份为在线快照，各存储不是跨存储原子事务；未新增长期定时备份或自动删除策略。

迁移失败不启动新应用；目标或切流验收失败保留/恢复旧 AI upstream，数据库不自动回滚。新色已验收后若旧色排空失败，CI 返回失败并保持新入口，避免切回已经停止的旧色。并发 Caddy 修改会阻止覆盖。数据库迁移仍要求向后兼容。

## 仓库与在线 workflow 的关系

本地已更新 `.github/workflows/release.yml` 为直接调用 `longdao-deploy`，新增发布器、回归测试及运维文档，**未提交或推送代码**。

线上已发布 workflow 仍包含旧的 shell heredoc，但使用新 GitHub 变量、密钥及新机受限 SSH 入口：入口只接受固定 `/www/longdao` 路径和合法版本，丢弃旧 heredoc，调用新发布器，绝不执行旧栈脚本。已通过真实 GitHub job 验证，因此自动部署现在已经迁移生效，不依赖先提交本地新版 workflow。历史标签重跑也经过同一入口。

兼容性测试向旧格式 SSH 命令传入 `exit 99`，新入口仍正确完成当前版本验证，证明 stdin 未执行；同一部署密钥尝试 `docker ps` 被明确拒绝。

本地代码：

- [新机发布器](../deploy/production/huawei_release.py)
- [SSH 入口](../deploy/production/huawei_ci_entry.py)
- [回归测试](../deploy/tests/test_huawei_release.py)
- [运维手册](../deploy/production/HUAWEI.md)
- [新版 workflow](../.github/workflows/release.yml)

私密备份及 GitHub 验收证据：`/Users/yao/.codex/backups/longdao/20261103-cicd`，不入 Git。旧机仍需保留以支持旧域名转发；不能因自动部署迁移完成就释放旧服务器。
