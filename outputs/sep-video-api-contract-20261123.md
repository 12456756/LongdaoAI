# SEP 视频接口契约（脑生云旧协议）

本次修改仅覆盖 `naoshengyun_legacy` 视频协议。原生视频协议保持现有转发和计费方式；其任务不支持本次新增的取消能力，返回具体的 `unsupported_video_operation`。

2026-11-24 补充：错误及成功 JSON 的统一回执、前置鉴权错误和本地失败记录恢复，以 [完整回执契约](sep-media-receipt-contract-20261124.md) 为准；原生视频也补齐回执元信息，不改变其按原方式计费的时点。

## 接口与身份

| 方法及路径 | 含义 |
|---|---|
| `POST /v1/videos` | 创建，文生视频 JSON、图生视频 multipart 格式保持不变 |
| `GET /v1/videos/{task_id}` | 按任务 ID 向原账号查询一次 |
| `GET /v1/videos/{request_id}?id_type=request_id` | 先查询本地提交记录；已有任务 ID 时向原账号查询一次 |
| `PUT /v1/videos/{task_id}/cancel` | 无请求体，向原账号取消一次 |
| `GET /v1/videos/{task_id}/content` | 原下载入口 |
| `GET /v1/sub2api/videos/usage?request_id={request_id}` | 本地扣费证据，不查询上游、不触发生成 |

上述接口均使用创建时的同一把 Longdao Key。请求标识、任务 ID均不代替鉴权。同一用户换 Key 也不能操作原任务。

SEP 创建前生成唯一标识，放入 `X-Client-Request-ID` 与 `Idempotency-Key`，建议两者相同。前者允许 `[A-Za-z0-9_.:-]`、1 至 256 字符；后者最大 255 字符，所以两者相同时最长 255 字符。不提供标识的旧客户端仍可创建，但连接断开时可能不知道平台生成的标识。平台回显 `X-Client-Request-ID`；查询、取消等响应另有 `X-Video-Request-ID` 表示原创建标识。

`id_type` 默认为 `task_id`；只接受 `task_id` 和 `request_id`，不支持重复参数。按请求标识查询只支持 GET 状态接口。

## JSON 返回字段

旧协议 JSON 返回 `schema_version: 2`。正常创建仍为 HTTP 202，正常查询、取消为 HTTP 200。保留已有 `object/id/model/status/error`。错误响应保留 `error`，增加以下公共字段。

| 字段 | 含义 |
|---|---|
| `request_id` | 原创建标识；查询失败且无法关联时可能暂为本次调用标识 |
| `task_id` | 上游任务 ID，未取得时为 null |
| `operation` | 当前调用：create/query/cancel/download |
| `submission_status` | not_submitted/submitting/rejected/accepted/failed/protocol_error；旧历史记录可能仍含 unknown；failed 表示本地创建操作已结束，不代表上游任务执行失败 |
| `submission_retry` | 创建重试证据；attempts 为已尝试次数，max_attempts=4，exhausted 表示三次追加重试已耗尽；errors 保留每次失败的脱敏原因及 attempt 序号 |
| `status` | queued/in_progress/completed/failed/cancelled；没有已确认状态时为 null |
| `is_final` | 任务是否终态，不是账务是否终态；未确认时为 null |
| `status_observed_at` | 最近确认时间，Unix 毫秒；旧数据没有时间时为 null |
| `status_source` | upstream 为本次查询确认，cached 为本地最近记录；无任务状态时为 null |
| `task_error` | 生成任务自身错误，与查询/取消操作错误分开；无则 null |
| `error` | 当前接口失败原因；按请求标识读取失败提交记录时为原创建失败原因，`error.operation=create` |
| `billing.status` | pending/settled/not_charged/unknown |
| `billing.actual_cost_usd` | 金额字符串；确认不扣费为 0，未结算/无法确认则 null |
| `billing.error` | 平台结算或读取账务失败的具体原因，可独立于生成结果存在 |

查询记录成功并不表示生成成功：可能 HTTP 200、`submission_status=rejected`、`task_id=null`，正文给出原创建失败。下载成功是二进制，不带 JSON 正文，响应头 `X-Video-Billing-Status` 表示账务状态；需要金额和诊断时查询用量接口。

错误保留 `error.source/code/type/message/platform_code`，供应商字段同时放入 `error.upstream`，包括原 HTTP 状态、错误码、reason、param、details、请求 ID等已有信息。`error.operation` 标识真正失败的操作。仅脱敏凭证、Cookie、连接串、个人信息及内部敏感片段。上游提供时回传安全的 `Retry-After` 和 `X-Upstream-Request-ID`。不要仅凭对外 502 推断提交或扣费状态。

## 提交结果与客户端行动

| 情况 | 返回与平台行为 |
|---|---|
| 本地校验、报价、存储等在发送前失败 | not_submitted，具体平台原因；尽可能保存失败记录供查询 |
| 上游明确参数、鉴权、余额、限流等 4xx，或 HTTP 200/202 的明确业务拒绝 | rejected，不重试，保存具体上游原因，本次关闭为不扣费 |
| 202 且业务回执有效 | accepted，保存原任务 ID；queued/processing 不最终扣费 |
| 断线、超时、无法解析或缺少任务 ID 的回执，以及没有任务 ID 的 HTTP 5xx | 同一调用内追加最多三次幂等重试；最终仍无有效任务 ID 则 failed/not_charged，金额为 0 |
| 200/202 中业务错误 | 不能按 HTTP 成功处理，返回具体业务错误 |
| 回执模型、身份或状态不一致 | 平台协议错误，保存可核对的任务 ID，不虚构生成结果 |

Longdao → 脑生云：`POST /v1/video/tasks`。首次提交加三次追加重试，最多四次，间隔为 1、2、4 秒。每次请求最多 60 秒，整段上游提交预算为 247 秒（不含前置处理和本地存储收尾）；客户端和反向代理应为创建接口留出该等待时间。客户端断开不会取消已经开始的这段有限重试，结果仍写回原提交记录。相同上游账号、模型、Authorization、原参考图签名 URL、完整 JSON 和 Idempotency-Key 全部复用；不重传图片、不切换渠道、不后台轮询。

取得合法任务 ID 后即停止追加创建请求。新版记录在回执其他字段不合法时返回具体协议错误，submission_status=protocol_error，保留任务 ID供后续核对，并关闭用户扣费；不假装上游任务执行失败。请求标识不能替代脑生云任务 ID。

SEP → Longdao：路径和请求体不变。相同 Key、相同幂等键、相同有效参数，已受理返回原任务，已失败返回原失败及原重试次数；不会重新启动第二轮四次提交。不同参数返回 HTTP 409 `idempotency_conflict`。在途重复请求返回 409 `video_submission_in_progress`，submission_status=submitting。创建连接丢失时可按原 request_id 查询，无需更换幂等键创建新任务。

重试耗尽时 HTTP 状态及 error.message/code/details 保留最后一次具体错误，不统一替换文案；submission_retry.errors 保留此前各次原因。例如连续连接中断返回：

```json
{
  "schema_version": 2,
  "request_id": "sep-original",
  "task_id": null,
  "operation": "create",
  "submission_status": "failed",
  "status": null,
  "is_final": null,
  "submission_retry": {
    "attempts": 4,
    "max_attempts": 4,
    "exhausted": true,
    "errors": [
      {"attempt": 1, "source": "platform", "code": "video_upstream_error", "message": "Send video upstream request: unexpected EOF"},
      {"attempt": 2, "source": "platform", "code": "video_upstream_error", "message": "Send video upstream request: unexpected EOF"},
      {"attempt": 3, "source": "platform", "code": "video_upstream_error", "message": "Send video upstream request: unexpected EOF"},
      {"attempt": 4, "source": "platform", "code": "video_upstream_error", "message": "Send video upstream request: unexpected EOF"}
    ]
  },
  "billing": {"status": "not_charged", "actual_cost_usd": "0"},
  "error": {"source": "platform", "code": "video_upstream_error", "message": "Send video upstream request: unexpected EOF", "operation": "create"}
}
```

上例省略了其他 nullable 字段及部分错误字段；以实际具体错误为准。本地创建 failed、task_id=null 已足以关闭本次提交，不需等待不存在的任务终态。上游可能自行受理或产生费用，不转嫁给用户。只有账务数据库本身无法确认关闭结果时，billing 才可能为 unknown，并带 source=platform 的具体 billing.error；生成错误和平台账务故障分别表达。

提交记录包含固定截止时间（247 秒提交预算外加 10 秒宽限）。服务中途退出时，后续状态查询或同键重复 POST 会将超过截止时间且无任务 ID的记录关闭为 failed/not_charged，不恢复派发；旧版已保存的无任务 ID的 unknown 也按此关闭。通过 Redis 原子比较更新，避免并发关闭覆盖已经保存的受理回执。

旧数据没有原请求索引时，只能继续按已有任务 ID查询。Redis 过期、丢失或不同 Key 查询返回查询错误，不证明上游没有任务或没有扣费。7 天本地保留不表示上游有 7 天幂等保障。

## 查询、取消和下载

每次状态查询只调用原上游账号一次。查询 HTTP 错误是查询失败，原已确认状态保持不变，仍返回 cached 状态及独立操作错误。终态在 Redis 原子锁定，不被迟到的非终态或冲突结果覆盖；失败原因随终态保存。

取消成功必须由上游确认 canceled/is_final=true。409、其他错误和超时均返回实际原因，不自动追加查询，由客户端决定查询。已知 canceled 的重复取消直接返回本地取消结果。原生协议取消明确返回 422，不能误发 GET 冒充取消。

下载仍使用原下载链路，包括原先已存在的临时链接失效刷新一次；不增加生成重试。获取任务终态时同样执行统一结算。文件暂不可下载不等于生成失败，不能将成品下载错误冒充任务失败。

## 资金与保留

创建保存服务器报价快照，确认 succeeded 后按原快照结算一次，failed/canceled/明确创建拒绝/无回执重试耗尽不扣费；重试不额外登记账务或扣费。成功查询不依赖当前新报价。任务成功但本地扣费失败，仍返回 completed 并在 billing 内明确失败；客户端再次查询可重新请求本地结算。

新数据库表 `video_request_billing` 只保存财务身份、报价指纹、规格与结果。成功扣费及 settled 更新在现有扣费事务内完成；不扣费关闭与扣费争用同一行锁，不能同时成立。已关闭 not_charged 的记录不会因迟到成功自动补扣。历史任务无新快照时只读取已有账务证据，不重新扣费。

在参数校验等极早阶段拒绝的请求尚未进入财务登记。2026-11-24 新增原 Key 隔离的 Redis 失败回执后，已保存记录的创建响应、按请求标识查询和独立用量查询均能明确 not_submitted/not_charged。用量查询优先读取数据库账务及扣费证据；旧请求未保存失败回执时，仍不能把没有账务记录普遍解释成金额 0。完整统一回执见同目录 sep-media-receipt-contract-20261124.md。

任务路由、提交记录、原 Key 绑定和错误在 Redis 保存 604800 秒，不因查询续期。金额证据在 PostgreSQL 中独立保存，不跟 Redis 一起到期。用量接口升级 schema_version=2；新记录明确 pending/settled/not_charged，旧数据没有可靠证据则 unknown（原先长期返回 pending 的情况改为 unknown），金额不能据此填 0。

不新增长期资金冻结、自动免单期限或后台确认流程。因此调用方从不查询且从不下载时，平台无法及时发现生成终态并结算；此为本次约定的调用驱动方式。上游自身异常扣费不直接转成用户费用。

账务 unknown 是平台证据不足或平台账务异常的表达，不是脑生云的任务状态。数据库不可用、账务记录缺失、身份/报价快照不匹配等仍应修复；不能为了消除 unknown 而直接把可能已提交的扣费事务声明为金额 0。旧历史记录缺少可靠账务证据也不等于确认未扣费。

## 升级与验证

随版本自动执行新增迁移 `182_video_request_billing.sql`；未执行迁移时不允许新的旧协议任务绕过财务登记发送。无需新增环境变量。没有修改 SEP 代码或生产数据。

验证使用模拟 HTTP 上游、Redis 测试实例、SQL mock 和专门的临时 PostgreSQL 测试实例。不调用付费模型。核验创建拒绝、无回执、查询恢复、取消冲突、失败诊断、所有权、原报价结算、重复扣费和取消/扣费竞争。

本轮新增验证：1/2/4 秒梯度、四次相同 JSON/幂等键、重试恢复、重试耗尽不扣费、重复 POST 不再派发、请求连接取消后继续有限重试、服务中断记录关闭、关闭账务失败的具体平台原因及再次查询恢复。官方依据为脑生云创建视频任务文档 `499628413e0` 的 Idempotency-Key 与“任务已创建或命中幂等结果”说明；未进行收费模型验证。
