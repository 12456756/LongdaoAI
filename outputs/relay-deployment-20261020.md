# 视频接口生产发布：2026.10.20-1

用户已明确要求部署。时间均为北京时间（Asia/Shanghai）。

## 发布结果

- 2026-10-20 11:37:28 完成蓝绿切换；当前 blue 健康，旧 green 保留并正常退出（exit 0）。
- 镜像：`ghcr.io/yaoruiquan/sub2api:2026.10.20-1`。
- 源码：`a448f519de5a26494615c7b9cfdc60381223fc85`，仅从该提交的干净 Git 归档构建；工作区其余未提交代码未纳入。
- 镜像 ID：`sha256:5fd5c6fc27cbc44f6d6058fdd8f637ea86f335e163059e946fd0bce2a75f1dcd`。
- 源码归档 SHA256：`3ea4852bfe640fd58e90002a218eff2826057f11aead8d644820421023ebec2f`。
- 发布目录：`/opt/longdao/releases/2026.10.20-1/`；构建日志 `build.log`。
- `.env` 已持久化 `IMAGE_TAG=2026.10.20-1`；`.active_color=blue`。
- 未推送 Git 或镜像仓库；未修改上游账户、商业价格或分组权限。

## 发布范围

新增 OpenAI 兼容视频创建 `POST /v1/videos`、别名 `POST /v1/video/generations`、状态查询及内容下载。包含原账户任务绑定、密钥隔离、媒体权限、视频计费，以及前端账户“视频生成”端点能力和 OpenAI 分组视频价格配置。

使用现有生产 Compose，显式指定 `sub2api-blue`、`--no-deps`，健康及冒烟检查后优雅停止 green。数据库迁移文件与生产相同，没有独立执行迁移；共享 PostgreSQL、Redis、Caddy 均未重建、重启或改配置。其他 SEP 容器未操作。

## 验证

- 干净发布源码的视频 service/handler 专项回归通过；完整 routes 测试通过。
- 同一份干净源码的 `TestGrokOAuthCredentialFailoverAcrossHTTPHandlers` 通过。此前工作区的聊天测试失败不在本次发布源码中。
- 镜像内程序显示 `Sub2API 2026.10.20-1 (commit: a448f519)`，镜像 revision 标签匹配完整提交号。
- 新实例现有冒烟通过：`/health`、首页 200，注册 GET 404。切换前脚本版本横幅读取旧 `.env`，不代表新实例仍是旧版本；镜像及程序版本已单独核验。
- 2026-10-20 11:38:18 复核外网 `/health`、`/`、`/admin/accounts` 均 200。
- 外网视频提交的两个路径、状态查询和内容下载共四个路由均返回预期未认证 401，没有请求付费上游。
- 新实例实际提供的 `AccountsView-DXrG1tpV.js` 含视频能力选项、复选框和视频能力持久化逻辑。
- 该检查时刻，发布启动后已有 36 条正常用量记录，最近一条时间 11:38:16。
- 新 blue、Caddy、PostgreSQL、Redis 重启计数为 0，无 OOM；旧 green 正常退出。
- 浏览器访问未获权限，未进行登录后的可视化点击验证。没有在生产新建测试账号或再次发起付费视频生成；上游真实生成结果另见 `a6api-video-minimax-h3-20261020/verification.md`。

## 备份与回滚

远端私有备份：`/opt/longdao/releases/2026.10.20-1/rollback/`，包含数据库、应用数据、原 env、活跃色、Compose 和 Caddy 配置。数据库 custom-format dump 经 `pg_restore -l` 校验成功。

数据库及应用数据已另存本机 `/Users/yao/.codex/backups/longdao/2026.10.20-1/rollback/`，权限 600，两端 SHA256 一致：

- `sub2api.dump`：`ec028751ace85412840b52951698a861bbfa9bbebf63509835513eff5889d77d`。
- `app-data.tar.gz`：`3ce29b5b2aa59ca95700366831fc0e15eb1185bedc43b6b89ce093c4ab35d898`。

旧版本 green 镜像 `ghcr.io/yaoruiquan/sub2api:2026.10.15-2` 保留。回滚先启动 green 并验证健康，再优雅停止 blue，使 Caddy 优先 blue 的配置转向 green；恢复备份中的 `.env` 与 `.active_color`，验证外网。无需删除卷、修改代理或恢复数据库。

## 配置提示

刷新管理页面后，在 OpenAI / API Key 账号弹窗下方“端点能力”中启用“视频生成”；分组需开启现有媒体权限并配置视频价格。Base URL 使用上游根地址；调用端必须实际使用视频接口。
