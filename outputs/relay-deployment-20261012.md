# 中转发布记录：2026.10.12-1

时间均为北京时间（Asia/Shanghai）。用户已明确要求部署生产中转。

## 发布结果

- 2026-10-12 20:39:58 完成蓝绿切换，活跃色为 green。
- 镜像：`ghcr.io/yaoruiquan/sub2api:2026.10.12-1`。
- 源码提交：`0ad9687b3a564fdaeb55bbde18980e68f6b21d89`。
- 镜像 ID：`sha256:3f58ffe77d08780f6acd114585bc353e0434271224b3a224cb894367dd0142ac`。
- 镜像在生产主机由已提交源码归档构建；没有推送 Git 或镜像仓库。
- `.env` 中 `IMAGE_TAG` 已持久化为新版本，`.active_color` 已更新为 green。

## 已上线的修复

1. 应用：会话级 `session_blocked_by_cyber_policy` 不再暂停共享账号；Responses 保留 403 和错误码，停止请求故障切换，正确终止已提交的 compact 流。
2. 代理：移除 Caddy 的 `unhealthy_status 500 502 503 504`，保留 `/health` 主动探活和连接失败的被动检查，避免业务错误连带摘除管理页面后端。

应用源码来自上述提交。本地和生产 `deploy/production/Caddyfile` 均已修改，但这次代理配置修改尚未提交 Git。

## 操作边界

- 使用独立发布目录，不覆盖生产仓库的其他已有改动。
- 通过 Compose 显式指定 green 应用并使用 `--no-deps` 启动，未重建共享 PostgreSQL、Redis 或 Caddy。
- 先检查 green 健康、首页、管理页面和现有冒烟，再热加载已验证的 Caddy 配置；等待探活后让旧 blue 实例优雅停止。
- 旧 blue 容器及镜像均保留，未删除数据卷。
- 应用补丁没有数据库迁移文件变更；没有另行运行独立数据库迁移。

## 验证

- 新镜像版本输出和 revision 标签均对应 `0ad9687b`。
- Caddy 候选配置校验成功，热加载成功；读取管理 API 确认实际配置已无 `unhealthy_status`。
- 新实例的现有生产冒烟测试通过：健康接口 200、首页 200、注册 GET 返回 404。
- 外网 `/health`、`/`、`/admin/accounts` 连续三轮检查均为 200。
- 截至 20:42:16，切换后有四条 `gpt-6.1-sol` 成功 usage 记录；green 应用访问日志对应 `/responses` 返回 200。
- 20:40:00 至 20:42:16 的 ops 错误表查询无记录。
- GPT 账号 31 为 active、可调度，冷却字段为空。
- 应用 green、Caddy、PostgreSQL 和 Redis 均无 OOM，重启计数为 0。

没有在生产故意制造会话封锁。已有本地回归测试验证该错误的修复行为；没有自动验证管理员 TOTP、支付或全量计费流程。

## 备份与回滚

发布前数据库 custom-format dump、应用数据及配置备份：

```text
/opt/longdao/releases/2026.10.12-1/rollback/
```

数据库 dump 的目录列表校验成功。数据库和应用数据备份另复制到本机，文件权限为 600：

```text
/Users/yao/.codex/backups/longdao/2026.10.12-1/
```

旧应用：`longdao-sub2api-blue`，镜像 `ghcr.io/yaoruiquan/sub2api:2026.09.10-4`，目前停止但保留。

如需回滚，先启动保留的 blue 并检查健康。Caddy 当前 `lb_policy first` 且 blue 排在前面，blue 恢复健康会优先接流量；验证后再优雅停止 green，恢复 `.active_color` 和 `.env` 的旧值。代理原配置保存在 rollback 目录，可独立恢复并校验、热加载。无需删除数据卷或恢复数据库。

## 剩余问题

观察到渠道监控写历史记录时报 PostgreSQL UTF-8 编码错误；已核对旧 blue 实例在发布前也存在相同错误，不是本次补丁引入。它影响渠道监控历史写入，不属于本次账号调度和管理页面 503 修复范围。
