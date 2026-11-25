# Longdao 异步图片接口

日期：2026-11-23。适用脑生云文档中不带“新”的“GPT 图像生成”协议。

2026-11-24 补充：所有 JSON 错误出口、完整 nullable 字段以及早期失败后的用量查询，以 [完整回执契约](sep-media-receipt-contract-20261124.md) 为准。非 2xx 的 error 与 operation_error 表达本次操作错误，task_error 保留原任务失败原因。

## 调用方向与配置

| 方向 | 方法和路径 | 用途 |
|---|---|---|
| SEP → Longdao | POST /v1/images/generations/async | 文生图 |
| SEP → Longdao | POST /v1/images/edits/async | 图生图 |
| SEP → Longdao | GET /v1/images/tasks/{task_id} | 查询状态、下载地址和账务 |
| SEP → Longdao | GET /v1/images/tasks/{request_id}?id_type=request_id | SEP 没收到创建回执时，按原请求标识找回原任务与提交结果 |
| SEP → Longdao | GET /v1/images/tasks/{task_id}/content | Longdao 下载成品并转发图片文件 |
| Longdao → 脑生云 | POST /v1/images/generations/async | 两类任务共用 JSON 提交接口 |
| Longdao → 脑生云 | GET /v1/images/jobs/{jobid} | 固定用创建任务的账号查询 |

Longdao 上述接口也提供无 `/v1` 的别名。同步图片接口保持原行为。

在 Longdao 管理后台创建或编辑 OpenAI API Key 账号，将“异步图片协议”设为“脑生云 GPT 图像生成（不带新）”。凭证字段为 `image_protocol=naoshengyun_gpt_async`。默认 `background_sync` 保持旧有后台执行同步图片的方式。脑生云异步分支不需要配置 `image_storage`。

异步分支只选择支持此协议及请求模型的账号；没有合适账号时返回具体错误，不切换为同步生成。账号中的模型映射仍生效，不自动删改 `-1k/-2k` 等名称后缀。

发布时由项目现有迁移机制执行 `183_image_request_billing.sql`，创建图片账务证据表；它不存图片文件。

## 创建

请求头：`Authorization: Bearer <Longdao Key>`、`Content-Type`、`X-Client-Request-ID`、可选 `Idempotency-Key`。

请求 ID 和幂等键允许 1～256 个英文字母、数字及 `_.:-`；省略请求 ID 时使用网关生成的请求 ID，省略幂等键时使用请求 ID。重复请求依赖调用方保持稳定标识；不同标识会被视为不同生成请求。

SEP → Longdao 文生图示例：

```http
POST /v1/images/generations/async
Authorization: Bearer <Longdao Key>
Content-Type: application/json
X-Client-Request-ID: sep-image-example-1
Idempotency-Key: sep-image-example-1
```

```json
{
  "model": "gpt-image-2-1k",
  "prompt": "海边日出",
  "n": 1,
  "size": "1024x1024",
  "quality": "medium",
  "response_format": "url"
}
```

图生图使用 SEP → Longdao `/v1/images/edits/async`。multipart 重复上传 `image[]` 文件字段，其余字段与文生图相同；Longdao 将文件转换为 data URI，作为 JSON `image_urls` 传给脑生云。也支持 JSON 的 `images:[{"image_url":"..."}]`，以及单独使用 `image_urls`、`image_url` 或 `image` 别名，不允许混用。

| 限制 | 数值或规则 |
|---|---|
| 输出数量 | n=1 |
| 输出尺寸 | 省略时为 1024x1024；必须能确定计费尺寸，不接受 auto |
| 参考图数量 | 1～4 张，不截断、不拆单 |
| 单张参考图 | 最多 20,971,520 字节（20 MiB），PNG/JPEG/WebP |
| 上传图片像素 | 最多 100,000,000 像素 |
| multipart 请求 | 最多 85,983,232 字节（82 MiB，含封装） |
| 单个 multipart 文本字段 | 最多 65,536 字节 |
| JSON 和转换后的上游 JSON | 最多 115,343,360 字节（110 MiB）；网关全局上限更小时取更小值 |
| 输出方式 | 图片文件通过 Longdao content 接口下载，response_format 省略或为 url |
| 不支持参数 | stream、mask、style、input_fidelity、partial_images，以及未声明字段 |
| 提交及上游查询超时 | 每次最多 60 秒 |

图片大小沿用现有图片模块的 20 MiB 单文件上限，上传文件和 data URI 会由 Longdao 校验；直接传远程参考图 URL 时，由脑生云负责获取和验证其内容。data URI 中的 Base64 会增加请求体大小，使用完整编码后大小进行校验。图生图参考文件不落盘、不写 Redis、不记录图片正文。

成功提交返回 HTTP 202；仅接受脑生云 HTTP 202 且任务 ID、object、状态、创建时间有效的回执。

```json
{
  "schema_version": 2,
  "id": "imgtask_example",
  "task_id": "imgtask_example",
  "object": "image.generation.task",
  "request_id": "sep-image-example-1",
  "submission_status": "accepted",
  "status": "processing",
  "upstream_status": "queued",
  "created_at": 1795392000,
  "expires_at": 1795478400,
  "poll_url": "/v1/images/tasks/imgtask_example",
  "billing": {"status": "pending", "actual_cost_usd": null}
}
```

响应头：`X-Client-Request-ID`、`Location`（查询地址）、`Retry-After: 3`、`Cache-Control: no-store`。

同一 Key 下，请求 ID 或幂等键命中相同参数，返回原任务；参数冲突返回 409 `image_request_conflict`。原请求正在提交时返回 409 `image_submission_in_progress`，不重复派发。原请求提交失败时返回原失败，不重提；需要新的生成任务时使用新的请求 ID 和幂等键。

创建明确报错、HTTP 200 业务错误、超时、断线、空回执或无效任务 ID，均按本次创建失败结束，不扣用户费用，不冻结余额，不阻塞后续任务。上游自行创建或扣费不转嫁给用户。算力平台不自动重提、切换渠道或后台查询。

2026-11-23 重新核对脑生云“GPT 图像生成”的异步提交官方文档 `500889114e0`：OpenAPI 为 `parameters: []`，没有声明 Idempotency-Key 或同键去重重试保证。不能套用豆包视频的幂等重试能力，因此本图片协议维持一次上游提交，失败即关闭为不扣费。SEP → Longdao 接收的 Idempotency-Key 只用于 Longdao 本地重复请求识别，不代表脑生云支持该请求头。脑生云以后明确提供该保证时再接通上游创建重试。

若进程在提交中退出，本地记录在 60 秒提交期限外增加 10 秒保存宽限，后续查询或重复提交会将过期的提交记录关闭为失败，不再次向上游派发；这是提交保护，不是后台恢复任务。

向脑生云开始提交后，Longdao 不因 SEP 的 HTTP 连接取消而取消这一次上游提交，仍执行有期限的单次创建并保存收到的回执。上游请求受 60 秒提交截止时间限制，保存结果最多使用额外 10 秒。无需新增图片重试，也不会因为 SEP 再来查询而重新创建任务。请求在登记前被拒绝或连接取消时，不保证存在可查询记录。

## 查询

SEP → Longdao：GET `/v1/images/tasks/{task_id}`，必须使用原 Longdao Key。同一用户的另一把 Key 也不允许查询或下载，返回 404。

### 按原请求标识恢复创建结果

SEP 没收到 Longdao 的图片任务 ID 时，使用创建时已提供的 `X-Client-Request-ID` 值：

```http
GET /v1/images/tasks/sep-image-example-1?id_type=request_id
Authorization: Bearer <创建时的 Longdao Key>
```

这只查询 Longdao 本地索引，取得原 Longdao 任务；如果任务已受理且还在处理中，再按原账号查询脑生云。请求标识不能代替脑生云任务 ID，查询不会调用上游创建接口。`id_type` 默认 `task_id`，显式也支持 `task_id`；只接受一个参数值，其他值、空值或重复值返回 400。request_id 限制与创建一致：1～256 个英文字母、数字及 `_.:-`。严格查询 request 索引，不将相同文字的幂等键当作 request_id。下载只接受 Longdao task_id，`/content?id_type=request_id` 返回 400。

| 本地记录 | HTTP 与返回信息 |
|---|---|
| 正在提交 | 200，submission_status=submitting、status=processing、本地 task_id、原 request_id、billing；Retry-After: 3，不调用上游查询或创建 |
| 已受理 | 200，submission_status=accepted，原 task_id、实际任务状态、账务及成功后的 content_url；沿用已有上游查询和结算流程 |
| 本次创建失败 | 200，submission_status=failed、status=failed、原具体 error、billing.status=not_charged、金额 0；账务关闭故障另在 billing.error 表达 |
| 提交已超时中断 | 后续查询关闭为 failed/not_charged，保留 image_submission_interrupted 具体原因，不重提 |
| 记录不存在、已过期或使用另一把 Key | 404 image_request_not_found；不表明上游没任务或用户没扣费，不虚构金额 0 |
| Redis 读取失败 | 503，保留具体平台原因，不返回“记录不存在” |
| 脑生云查询失败 | 沿用非 2xx 与 operation_error；同时保留已恢复的原 task_id、request_id 及最近确认状态 |

按任务 ID 与按请求标识查询使用相同图片任务响应结构，`X-Image-Request-ID` 指向原创建请求，原任务仍使用 `/v1/images/tasks/{task_id}` 查询、`/content` 下载。请求标识索引与任务均在 Redis 保留 24 小时，不因查询续期；不同 Key 不允许恢复。HTTP 200 只代表本次查询成功，不能忽略 submission_status/status/billing。生成前参数、报价、权限等前置拒绝可能未登记任务，404 不构成没有收费的证据。该入口仅覆盖脑生云异步图片协议，既有 background_sync 任务仍按原 task_id 查询。

| 脑生云状态 | Longdao status | upstream_status |
|---|---|---|
| queued | processing | queued |
| running | processing | running |
| succeeded 且有一个有效图片 URL | completed | succeeded |
| failed | failed | failed |

处理中响应带 `Retry-After: 3`；成功响应增加：

```json
{
  "task_id": "imgtask_example",
  "request_id": "sep-image-example-1",
  "submission_status": "accepted",
  "status": "completed",
  "upstream_status": "succeeded",
  "content_url": "/v1/images/tasks/imgtask_example/content",
  "image_url": "/v1/images/tasks/imgtask_example/content",
  "result": {"data": [{"url": "/v1/images/tasks/imgtask_example/content"}]},
  "billing": {"status": "settled", "actual_cost_usd": "0.120000000000"}
}
```

`result` 内的 URL 同样是需要原 Key 鉴权的 Longdao 下载地址。脑生云 URL 只保存在内部任务记录，不对外返回。金额只是格式示例，不代表模型价格。

查询接口 HTTP 200 表示查询成功，不等于生成成功。`status=failed` 时，`error` 是上游任务失败原因。查询操作报错时使用非 2xx，`operation_error` 说明查询错误，响应保留最近确认的任务状态；不会因查询 404、超时等把生成任务改成失败。

对上游非 2xx、业务错误、纯文本、错误详情和请求 ID 保留诊断信息，遮蔽敏感片段。上游认证/任务不存在等错误由 Longdao 包装为 502，正文保留 `upstream_status` 和具体原因；它不是调用方 Longdao Key 无效。

任务的路由关联、幂等索引、状态和成品源 URL 在 Redis 保存 24 小时；查询不无限延长期限。已确认终态读取缓存。24 小时不是脑生云成品 URL 的有效期承诺。过期后查询返回 404。

## 下载

SEP → Longdao：GET `/v1/images/tasks/{task_id}/content`，携带原 Longdao Key。

Longdao 必要时查询原任务、确认生成成功并处理一次性结算，然后请求保存的脑生云成品 URL，以文件流转发图片。返回图片原始字节及真实 `Content-Type`；不返回 Base64、不保存整张图片到 Redis、不上传对象存储。

| 下载限制 | 数值或规则 |
|---|---|
| 文件大小 | 最多 20,971,520 字节（20 MiB） |
| 文件类型 | image/png、image/jpeg、image/webp，并校验文件签名 |
| 文件源地址 | 公网 HTTPS，443 端口；拒绝内网、回环、保留 IP及 URL 中的凭证 |
| 重定向 | 最多 3 次，每次校验目标并清除请求凭证 |
| DNS | 校验解析后的 IP并固定连接该 IP，避免 DNS 重绑定 |
| 总下载时间 | 最多 60 秒 |
| 单次源站读取停滞 | 最多 20 秒 |
| 并发 | 每个 Longdao 进程同时最多 4 个下载，无等待队列；超限 429 image_download_concurrency_exceeded |

Longdao 不将用户 Key、上游 API Key 或 Cookie 传给成品文件源。响应使用 `Cache-Control: no-store`、`X-Content-Type-Options: nosniff`，并提供 `X-Image-Request-ID` 和 `X-Image-Billing-Status`。

未生成完成返回 409 `image_not_downloadable`。源 URL 过期、文件源报错或下载失败，返回具体下载原因，生成状态仍保留 completed，不重复生成、不重复扣费。图片正文开始传输后断流，终止响应并记诊断日志，不拼接 JSON 污染图片字节。

## 账务

提交前冻结的是价格快照，不冻结用户余额。异步上游查询协议没有保证返回 token usage，因此该适配器要求可确定的按图/按次价格；只配置 token 价格而无法确定图片费用时，在派发前明确报错。

确认生成成功后结算一次，确认生成失败时记 not_charged；创建失败同样不扣费。账务证据保存在 PostgreSQL `image_request_billing`，使用与实际余额/额度扣减相同的事务及行锁，防止并发查询重复扣费。Redis 过期不删除账务证据。

账务 `pending/settled/not_charged/unknown` 与图片任务状态分别表达。账务异常放在 `billing.error`，不能把生成成功改成失败或阻止再次下载。没有后续状态查询或下载时，不后台轮询发现终态，也不会自动触发成功结算。

这里的账务 unknown 来自 Longdao 自身的账务查询、证据或结算异常（例如数据库不可用、记录/报价快照缺失、身份不匹配），不是供应商任务未知；billing.error.source=platform，message 保留具体原因。不能用 not_charged 覆盖无法确认的扣费事务结果。

SEP → Longdao 原有 GET `/v1/sub2api/usage?request_id=...` 对新异步图片优先返回这份账务证据，原同步请求仍使用原账务证据查询。传原创建响应的 request_id，而不是轮询请求的新 ID。

## 验证边界

本轮恢复接口新增模拟验证：创建时取消 SEP 连接后仍保存上游回执；按原 request_id 恢复同一任务及成品入口；提交中查询不访问上游；同用户换 Key 返回 404；request 与 idempotency 索引相互隔离；重复/非法 id_type 和按 request_id 下载返回 400；查询失败保留原任务；24 小时过期不虚构未扣费；Redis 故障返回具体 503；超时中断关闭不扣费；恢复后结算只执行一次；提交账务登记中保持 pending；迟到受理回执不会被超时查询覆盖或导致未取得账号就查询上游。

使用模拟脑生云、测试 Redis 和独立 PostgreSQL验证，不调用收费模型，不访问生产数据。公开协议来源：脑生云文档条目 `500889114e0`（异步提交）、`500889115e0`（查询）。未通过真实账号验证供应商服务开关、模型权限、四张参考图实际运行及成品链接寿命。

已执行：受影响 service/repository/handler/routes/migrations 包完整单元测试、服务构建、前端类型检查、相关 ESLint，以及创建/编辑账号组件的 49 个测试。模拟测试覆盖创建失败结案、业务错误保留、四张参考图转换、重复请求不重提、原 Key 隔离、终态缓存、下载字节转发及断流终止。独立 PostgreSQL 测试验证 16 个并发成功结算仅扣一次、失败结案不能再扣费、结算与不扣费关闭互斥。测试数据库容器已清理；未提交、未部署。
