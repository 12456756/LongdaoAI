# 中转故障排查与修复方案（2026-10-12）

所有时间均为北京时间（Asia/Shanghai）。本文不包含 API 密钥或账号凭据。

## 结论

本次 GPT 中转不可用的直接原因是：上游对一个会话返回 HTTP 403、错误码 `session_blocked_by_cyber_policy`，中转将其当成共享账号故障，暂停该账号调度 10 分钟。GPT 分组只有账号 31（codex）可用，暂停后其他用户的请求也因 `no available accounts` 返回 HTTP 503。

此外，原转发逻辑会对这种 403 尝试故障切换，并在部分路径把错误转成 502、丢失原始错误码，使客户端容易将确定的会话拒绝当成临时故障重试。

这次事件与之前 Codex 子代理使用上游不支持模型的配置问题不同。本次没有发现新的 Spark 模型错误。

## 生产证据

- 生产应用容器：`longdao-sub2api-blue`。
- 当前镜像：`ghcr.io/yaoruiquan/sub2api:2026.09.10-4`。
- 检查时容器运行约三周，重启次数 0，未发生 OOM；健康接口返回 200，主机资源无异常。
- GPT 分组 `group_id=3` 只有账号 31，账号类型为 OpenAI API key。
- 上游拒绝的结构化错误为：

```json
{"error":{"code":"session_blocked_by_cyber_policy","type":"session_blocked_by_cyber_policy","message":"当前会话被上游安全策略封锁，请新开会话后重试。…"}}
```

上面的 message 为脱敏摘要；code/type 为日志中实际错误标识。

| 时间 | 证据与结果 |
| --- | --- |
| 16:24:29.194 | 上游对一个会话返回上述 403。 |
| 16:24:29.203 | `openai_403_temp_unschedulable`，计数 1，阈值 3；账号已经被冷却至 16:34:29.196。 |
| 16:24:32.831 | 再次遇到相同 403，计数 2，冷却延长至 16:34:32.817。 |
| 16:24:42.916–16:34:29.812 | 记录到 147 次 HTTP 503，涉及 4 名用户；错误为无可用账号。 |
| 16:35 以后 | 成功请求恢复。 |
| 16:38:34.367 | 同类错误再次触发，计数 1，账号冷却至 16:48:34.357。 |
| 16:38:44.404–16:48:00.696 | 又记录到 117 次 HTTP 503，涉及 4 名用户。 |
| 17:04 复查 | 账号 active、可调度，冷却字段为空；最近 5 分钟 29 条成功 usage 记录，最后一条时间 17:04:06.851503；GPT 分组最近 5 分钟无 ops 错误记录。 |

两段错误窗口合计 264 次 HTTP 503。每段各涉及 4 名用户，未将人数相加推断为 8 名独立用户。受影响模型包括 `gpt-5.6-luna`、`gpt-5.6-terra`、`gpt-6-sol` 和 `gpt-6.1-sol`。

## 已完成的本地补丁

1. 只在 HTTP 403 且结构化 `error.code` / `error.type`（或 `response.error` 对应字段）明确等于 `session_blocked_by_cyber_policy` 时识别为会话拒绝。错误消息中仅提及该字符串不会命中。
2. 会话拒绝跳过账号临时不可调度规则、403 计数和账号状态修改，避免一个会话影响共享账号的其他用户。
3. 当前请求停止故障切换。Responses 普通路径与 passthrough 路径返回 403，并保留原始错误体和错误码；不再改写成 502。
4. compact 流式响应若已经由心跳提交 HTTP 200，则发送一个 `response.failed` 终止事件，保留会话错误码；内部记录预期错误状态 403。
5. Chat Completions 等兼容错误处理保留 403，以已有兼容格式返回 `permission_error` 和提示消息。该路径没有新增原始 `error.code` 字段。
6. 不把已被封锁会话的重复拒绝计为新的内容风控事件。普通账号权限失败仍保持原有账号保护逻辑。

该补丁不会解除上游会话封锁；被封锁的会话仍然被拒绝，用户需按上游提示新建会话。补丁修复的是错误作用范围扩大及错误状态被改写的问题。

变更文件：

- `backend/internal/service/openai_cyber_policy.go`
- `backend/internal/service/ratelimit_service.go`
- `backend/internal/service/openai_gateway_upstream_errors.go`
- `backend/internal/service/openai_gateway_passthrough.go`
- `backend/internal/service/ratelimit_service_403_test.go`
- `backend/internal/service/openai_session_policy_error_test.go`（新增）

## 验证

先新增回归测试，再修复逻辑。修复前测试复现了账号被暂停 10 分钟，以及策略返回临时不可调度；修复后通过。

在 `backend/` 目录执行：

```sh
# 会话策略、真实 Forward 转发、普通账号 403，以及 compact 流回归
go test -tags=unit ./internal/service -run 'Test(OpenAISessionPolicy_|RateLimitService_(HandleUpstreamError_(SessionPolicy|OpenAI403)|CheckErrorPolicy_SessionPolicy))' -count=1

# service / handler 全部 unit 标签测试
go test -tags=unit ./internal/service ./internal/handler -count=1

# 编译检查，输出到临时目录
go build -o /private/tmp/sub2api-session-policy-20261012 ./cmd/server
```

结果：以上全部通过。service 完整测试耗时 131.529 秒，handler 完整测试耗时 24.179 秒。`git diff --check` 通过。

回归测试覆盖 API key / OAuth、普通 / passthrough 转发、自定义临时不可调度规则、无 403 计数缓存、根级 / 包装错误字段、普通账号 403 不被误判，以及已提交 compact 心跳的流式终止行为。

没有在生产制造封锁会话，也没有进行部署后的线上复现验证。

## 上线与回滚方案

更新：2026-10-12 20:39:58 已按用户要求部署 `2026.10.12-1`，并同步调整代理业务错误健康检查。下文为排查时拟定的方案；当前发布结果见 `relay-deployment-20261012.md`。

当前补丁已完成本地验证，未推送、构建发布镜像或部署；生产仍运行旧版本，仍可能再次触发相同故障。

建议使用仓库现有的蓝绿发布流程：

1. 将已验证补丁制作为新的不可变版本镜像；确认镜像包含本次补丁，使用独立新版本标签。
2. 在非活跃色部署新镜像，保持旧活跃实例；检查健康、启动日志与既有冒烟测试。
3. 热切换应用流量至新实例，观察真实请求成功率、403/503、可调度账号数量及账号状态。避免对同一封锁会话进行自动循环重试。
4. 如遇回归，使用现有回滚脚本切回保留的旧实例。本补丁不修改数据库结构，不需要额外数据库迁移。

生产切换属于用户 AGENTS.md 中“外部生产变更时询问”的范围，执行前需要用户确认。本次仅完成生产只读排查与本地补丁验证。

## 后续建议

- 增加“可调度 GPT 账号数为 0”和 503 比例的告警。此次 `/health` 正常，单独检查进程存活无法发现推理服务不可用。
- 为真正的账号故障或上游服务故障准备第二个独立可用账号/上游，降低单一账号导致的整体不可用。会话封锁仍应直接反馈，不通过换账号绕过。
- 不建议直接取消所有 403 的账号保护：凭据无效、账号停用等真实故障仍需要正常处理。
