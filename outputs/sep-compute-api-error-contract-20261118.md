# SEP 调用算力平台的接口与错误返回对接说明

整理日期：2026-11-18。交付对象：SEP 后端、客户端及运维开发人员。

**版本边界**：算力平台依据 `D:\github\LongdaoAI` 本地 `main` 的错误信息完整性修复版本，具体版本以包含本文的本地提交为准，尚未推送或部署。SEP 依据本地 `D:\github\SEP`，提交 `843188688742ec360b4dfcea23ec310938726228`；`SEP-worktree-one` 的提交相同。本说明没有读取生产配置，也不能证明线上版本已具备本文行为。

本文只整理契约与接入建议，没有修改 SEP 或算力平台业务代码，没有实际调用模型。示例全部为人工构造，不是真实请求记录。

## 1. 已确认 SEP 使用的接口

表中使用完整 `/v1` 路径。SEP 多数适配器配置的 base URL 已含 `/v1`，拼接时不能重复添加。实际域名以部署配置为准，不在文档中假定某个域名正在使用。

| 编号 | 方法与算力平台路径 | SEP 使用场景 | 主要请求内容 | 正常响应与必须额外检查的内容 |
|---|---|---|---|---|
| A01 | `POST /v1/chat/completions` | 对话、数字员工、任务规划/交付、能力洞察、文案生成、模型测试、SEP 对外聊天网关 | JSON：`model/messages`，可含 `stream/tools/max_tokens` 等 | JSON Chat Completion 或 SSE；检查业务错误、终止事件与输出完整性。 |
| A02 | `POST /v1/embeddings` | 知识库向量化，限 SEP 配置为 sub2api provider 时 | JSON：`model/input/encoding_format` | JSON `data[].embedding`；错误对象和向量数量/维度错误不能按成功入库。 |
| A03 | `GET /v1/models` | 模型同步、图片/视频模型发现、健康检查 | 无请求体，携带对应连接的 Key | 通常 200 模型目录；检查 `partial/diagnostics` 和 `X-Data-Degraded`，目录存在不等于可生成或已配置价格。 |
| A04 | `GET /v1/sub2api/billing` | SEP 查询当前算力 Key 的 token 计费倍率 | 无请求体 | 200 `object=sub2api.key_billing`；重点读取 `effective_rate_multiplier`，失败不能自行按倍率 1 计价。 |
| A05 | `POST /v1/sub2api/images/pricing` | 生图、图生图报价 | JSON：`model/operation/size/quality/n` | 200 报价；必须检查 `pricing_status=quoted`、`amount_usd` 非 null、有效期及请求规格匹配。 |
| A06 | `GET /v1/sub2api/usage?request_id=...` | 单次图片生成账务核对 | URL 编码的账务关联 ID | 200 `pending/settled`；`pending` 不是生成中，也不证明未扣费。 |
| A07 | `POST /v1/images/generations` | 文生图 | JSON：`model/prompt/n/size/quality/response_format` | SEP 当前请求 `b64_json`，检查 `data[]` 与文件有效性；长等待时可能先提交 200，随后正文才是错误。 |
| A08 | `POST /v1/images/edits` | 图生图，多张参考图 | multipart：重复 `image[]` 文件字段，加 `model/prompt/n/size/quality/response_format` | 与 A07 类似；不能仅根据 2xx 判定图片生成成功。 |
| A09 | `POST /v1/sub2api/videos/pricing` | 文生视频、图生视频报价 | JSON：`model/operation/resolution/duration_seconds` | 200 报价；除图片同类检查外，兼容可选 `diagnostic`。 |
| A10 | `GET /v1/sub2api/videos/usage?request_id=...` | 单次视频生成账务核对 | URL 编码的账务关联 ID | 200 `pending/settled`，核对模型、数量、分辨率、时长、金额。 |
| A11 | `POST /v1/videos` | 文生视频、图生视频创建 | 文生 JSON；图生 multipart，重复 `input_reference` 图片文件字段；附报价和关联请求头 | 旧协议成功受理为 202 视频对象；原生协议响应取决于上游。受理成功仍须检查任务状态，不能当成已生成。 |
| A12 | `GET /v1/videos/{task_id}` | 查询已提交视频任务 | 路径中的任务 ID；使用创建时的同一 Key | 旧协议通常 200，`status=failed` 时仍为 200，真实原因在 `error`。 |
| A13 | `GET /v1/videos/{task_id}/content` | 下载视频成品 | 原任务 ID、原 Key | SEP 当前要求完整 200 视频字节；非 2xx 先按错误解析，已开始下载后的断流不能期待 JSON。 |

上述是**13 个不同方法/路径类型**，不是 13 个 SEP 功能。相同 `/models` 和 `/chat/completions` 会被多个业务调用。未发现 SEP 当前后端直接请求 Responses、Messages、异步图片或批量图片接口的证据；这些算力平台能力列在附录，不能当作已接入 SEP。

SEP 图片和视频可分别配置连接及 Key。聊天连接、图片连接、视频连接的模型目录、权限与计价结果不能互相代用。知识库 Embeddings 当前还可能从环境配置读取连接，与聊天读取系统设置的方式不同。

## 2. SEP 必须兼容的错误正文

目前返回格式不完全统一，不能只写 `body.error.message`。以下均是需要识别的实际格式类型。

### 2.1 鉴权和公共中间件：顶层 code/message

```json
{
  "code": "INVALID_API_KEY",
  "message": "Invalid API key"
}
```

上例 HTTP 401。这个结构没有 `error` 子对象，也不保证有 `source/type/param`。分组停用、额度用完等同样可能在公共中间件直接返回此结构。

### 2.2 OpenAI 类模型错误：error 子对象

```json
{
  "error": {
    "message": "Unsupported size; Bearer [redacted]",
    "type": "invalid_request_error",
    "code": "bad_size",
    "param": "size",
    "details": { "allowed": ["720p"] },
    "source": "upstream",
    "upstream_status": 422,
    "upstream_request_id": "example-provider-request-id"
  }
}
```

此示例对外可以是 HTTP 502，正文仍保留参数失败原因。`upstream_status=422` 与对外 502 分别表示供应商状态和算力平台状态，不能互相覆盖。

### 2.3 Anthropic 风格包装

```json
{
  "type": "error",
  "error": {
    "type": "permission_error",
    "message": "Image/video generation is disabled for this API key group",
    "source": "platform"
  }
}
```

**图片/视频报价、账务等 `/v1/sub2api/*` 接口也可能使用这种包装**，不能根据接口名称假定只返回 OpenAI 风格。

### 2.4 原生上游、代理及兼容返回

仍需兜底兼容：`{"error":"具体原因"}`、`{"message":"具体原因"}`、`{"detail":"具体原因"}`、JSON 字符串，以及纯文本。反向代理错误页也可能不是 JSON。对纯文本保留有界、脱敏后的原因；HTML 页面保留状态和安全摘要，不原样当作用户可执行 HTML 渲染。

Gemini 原生附录接口还可能返回 `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"..."}}`。供应商或视频任务的 `code` 也可能为数字，不能要求所有错误码都为字符串。

## 3. 错误字段与响应头

### 3.1 正文字段

以下字段均按“存在则保留”处理；不是每个错误都具备，不建议使用要求全部字段存在或拒绝未知字段的错误 Schema。

| 字段 | 类型/位置 | 含义和 SEP 处理方式 |
|---|---|---|
| `message` | 通常字符串 | 实际原因，供显示和排障；不要再次按 HTTP 502/503 替换成固定“服务不可用”。长文本展示可折叠，记录截断应有标记。 |
| `code` | 字符串或数字，可缺失 | 具体业务错误码；上游错误时通常是供应商码，平台错误时可能是本地码；保留原类型。 |
| `type` | 通常字符串，可缺失 | 错误类别，例如 `invalid_request_error/rate_limit_error/credential_error`；不保证每个来源使用相同枚举。 |
| `param` | 可为字符串、null 或其他 JSON | 涉及的参数；保留，不因类型不符合预期丢弃整条错误。 |
| `details`、`detail` | 任意 JSON 或字符串 | 允许的值、限制及其他结构化原因；两种命名都可能出现。 |
| `source` | 通常 `platform/upstream`，可缺失 | 算力平台视角的错误产生位置。SEP 是另一层平台，建议另存 `origin=longdao`，不要将这里的 platform 误标成 SEP 自身。 |
| `upstream_status` | 数字，可缺失 | 供应商实际 HTTP 状态；传输失败可能为 0 或没有此字段；视频任务内业务失败也不保证有此字段。 |
| `upstream_request_id` | 通常字符串 | 供应商请求跟踪 ID，用于上游排障；不是视频任务 ID或账务关联 ID。 |
| `request_id/requestId/requestID` | 供应商原始字段，可选 | 原文中的请求 ID，可能与补充的 upstream_request_id 同时存在；不要直接覆盖 SEP 主键。 |
| `platform_code/platform_message` | 可选 | 包装过程中附加的算力平台类别/上下文；不能覆盖真实供应商 code/message。例如下载时供应商失败码仍在 code，平台类别在 platform_code。 |
| `upstream_message` | 可选 | 包装后的 message 之外另行保留的供应商原始原因，例如因生成失败导致不可下载。 |
| `attempts` | 对象数组，可选 | 多次失败尝试的诊断。元素可带 message、diagnostic、upstream_status、stage、reason 等，不是固定只含 code/message。 |
| `stage/scope/reason` | 可选 | 失败发生阶段、影响范围和内部分类；不等于已经形成可供 SEP 自动退款的状态证明。 |
| `body_read_error` | 可选 | 读取供应商错误正文时还发生了异常；应与已读到的原始原因一并保留。 |
| `earlier_attempts_omitted` 等截断/遗漏标记 | 可选 | 记录受长度/次数限制，不能据此认为未列出的尝试从未发生。 |

### 3.2 响应头与关联 ID

| 字段 | 含义/限制 |
|---|---|
| `X-Request-ID` | 网关跟踪头，入口复用调用方提供的值或生成 UUID；部分上游透传分支可能写入供应商同名头。记录原值，不单凭此头判断任务或账务归属。 |
| `X-Client-Request-ID` | 算力平台客户关联 ID。当前公共中间件通常自行生成，不是简单回显 SEP 请求同名头；应读取**响应**值。旧协议幂等回执还可能恢复原提交的关联 ID。 |
| `client:<X-Client-Request-ID>` | SEP 当前媒体适配器使用的账务查询 request_id；冒号和 ID 整体进行 URL 编码。 |
| `X-Upstream-Request-ID` | 有供应商请求 ID 且尚可写头时补充；SSE/文件已经开始后不保证还能出现，需继续检查错误事件正文。 |
| `Retry-After` | 可为秒数或 HTTP 日期；存在才按实际解析，可能缺失。额度耗尽不能只靠等待解决，仍需看正文。 |
| `X-Video-Dispatch: rejected` | 当前视频报价保护出口的“尚未派发”标记，与 HTTP 409 及具体本地 code 一起判断。不是所有前置错误都带该头。 |
| `X-Data-Degraded: true` | 模型/用量查询等存在降级或部分数据，正文配合 partial/diagnostics。 |
| `Content-Type/Content-Length` | 区分 JSON、SSE、文件；文件下载必须完整读取并检查可信长度，不只检查响应头为 200。 |
| `Location` | 异步图片受理后用于查询；不要把它当成视频旧协议的公开供应商成品 URL。 |

视频 `task_id` 取创建响应的 `id`，后续用于路径 `/videos/{id}`。账务查询 `request_id` 优先使用媒体适配器取得的 `client:...`，不能拿视频 task_id 替换。SEP 自己的请求 ID、算力跟踪 ID、供应商请求 ID 和任务 ID 应分别保存。

## 4. 公共错误：所有接口都可能先遇到

以下是源码中可确认的本地分类，**不是所有供应商错误码的封闭全集**。`位置` 指通常出现于顶层 code 或 error.type/code；应按第 2 节兼容取值。

| HTTP | 本地标识/类型 | 实际原因 | SEP 建议 |
|---|---|---|---|
| 400 | `api_key_in_query_deprecated` | 在查询参数携带 Key | 改用 Authorization Bearer，不在 URL 中放 Key。 |
| 401 | `API_KEY_REQUIRED/INVALID_API_KEY/API_KEY_DISABLED` | 缺 Key、Key 无效、停用 | 检查该业务实际使用的连接，不循环重试。 |
| 401 | `USER_NOT_FOUND/USER_INACTIVE` | Key 所属算力用户不存在或不可用 | 联系算力平台管理员。 |
| 403 | `ACCESS_DENIED` | IP 访问控制拒绝 | 核对出口及访问规则，不认定为模型本身不支持。 |
| 403 | `GROUP_DELETED/GROUP_DISABLED/GROUP_NOT_ALLOWED` | 分组删除、停用或用户不再允许使用 | 修正分组或授权。 |
| 403 | `permission_error` | Key 未分组、媒体开关未开、模型或接口无权限等 | 使用具体 message 定位，不能统一描述为 Key 错误。 |
| 403 | `API_KEY_EXPIRED/SUBSCRIPTION_NOT_FOUND/SUBSCRIPTION_INVALID/INSUFFICIENT_BALANCE` | 到期、订阅或余额问题 | 处理对应账号/订阅/额度。 |
| 429 | `API_KEY_QUOTA_EXHAUSTED/USAGE_LIMIT_EXCEEDED` | Key 额度或订阅窗口额度用完 | 根据额度状态与重置时间处理，不能解释为并发繁忙。 |
| 429 | `rate_limit_exceeded/rate_limit_error` | RPM、并发、排队容量等限制；上游也可能返回此类 | 保留 message 和 Retry-After，区分配额与短期限流。 |
| 499 | `api_error`，`context canceled` | 部分并发等待分支发现客户端取消 | 连接已断时调用方通常收不到完整此响应；记录取消原因。 |
| 500 | `INTERNAL_ERROR/SUBSCRIPTION_MAINTENANCE_FAILED` 等 | 本地校验存储或订阅维护失败 | 保留原因与跟踪 ID，交由平台排障。 |
| 403 或 503 | `billing_service_error` | 既有状态映射下的账务服务故障 | 不仅凭 403 告诉用户余额不足，检查实际原因。 |
| 404 | `not_found_error/model_not_found` 等 | 路由、能力、模型或任务不可用 | 结合具体接口和 message 判断，不能都解释为任务不存在。 |
| 502/503/504 | `upstream_error/api_error/credential_error/timeout_error` 或供应商类型 | 上游拒绝、连接/协议/凭证/服务异常 | 读取正文真实原因；不要把所有 5xx 都自动当可安全重发的请求。 |

并发队列满的一个实际返回文案为 `Too many pending requests, please retry later`，并发名额不足可为 `Concurrency limit exceeded for <slot>, please retry later`。视频上传并发满可返回 `video upload concurrency limit reached (<N>)`。数字 N 来自配置，不在文档中假定生产值。

部分查询仍经过计费鉴权：当前仅 `/v1/usage`、`/v1/sub2api/billing` 和异步图片任务读取有明确跳过计费执行的逻辑。不要假定视频任务查询、视频下载或单请求 `/sub2api/*usage` 一定允许已过期/已耗尽额度的 Key。

## 5. 按接口处理

### A01 聊天及 A02 Embeddings

- 请求解析、参数或上下文限制等可能返回 400/413/422；模型/权限/容量选择可能为 403/404/503；上游错误按实际分支映射，既可能保留供应商状态，也可能转为 502。
- 保留 `error` 内诊断及公共顶层 code/message。HTTP 422 被映射为 502 时，仍应能展示例如“不支持 size 参数”，不能只展示“502 网络异常”。
- HTTP 200 仍检查显式业务错误；流式处理见第 6 节。Embeddings 必须验证输出数组及向量，再作为知识入库成功。
- SEP 中 SDK 对话、直接 fetch 的辅助生成和对外聊天网关是不同调用点，不能只修一个 Nest 异常过滤器就认定全部覆盖。

### A03 模型列表

普通成功通常为 `{"object":"list","data":[...]}`。仓库读取失败后继续返回后备目录时，当前修复加入如下额外信息：

```json
{
  "object": "list",
  "data": [],
  "partial": true,
  "diagnostics": [
    { "field": "models", "source": "platform", "message": "Database read timed out" }
  ]
}
```

实际 data 为当次后备目录，不保证是示例空数组。SEP 同步逻辑应识别降级标记，避免把一次后备结果当作完整权威目录，立即将原有模型全部标记消失；这属于 SEP 建议，不是算力平台已经替 SEP 实施的行为。健康检查获得 200 也不证明模型执行和报价链路正常。

### A04 倍率查询

成功返回 `object=sub2api.key_billing/schema_version=1/billing_scope=token` 及倍率。失败除公共错误外，还可能有本地 404 简化模式不支持、403 未分组、500 倍率服务不可用。失败没有供应商生成请求，不应标记为“供应商模型生成失败”。

### A05/A09 图片和视频报价

| 情况 | HTTP/正文 | SEP 处理 |
|---|---|---|
| 无效 JSON、未知字段、超过 4,096 字节 | 400，具体 Decode 原因或单对象校验原因 | 展示字段或格式问题；只修请求，不提交生成。 |
| 图片规格无效 | 400 `invalid_request_error`，涉及 model/operation/size/quality/n | 图片 operation 为 generate/edit；n 为 1..4；quality 为 low/medium/high，未传默认 medium。尺寸仍需满足平台计价规格。 |
| 视频报价规格无效 | 400，具体校验说明 | operation 为 text-to-video/image-to-video；resolution 为 480p/720p/1080p；duration_seconds 为 1..600。**这是报价范围，生成限制可能更窄。** |
| 分组媒体开关关闭 | 403 `permission_error`，`Image/video generation is disabled for this API key group` | 管理员开启媒体权限。 |
| 模型不允许 | 403 `permission_error`，message 包含模型/分组不可用原因 | 检查模型映射及授权。 |
| 配置、仓库或报价服务异常 | 通常 503 `api_error`，附可取得的具体原因；图片模型校验服务未初始化的既有分支为 403 `api_error` | 保存原因，请算力平台排障；不能仅按 403 归类为调用方权限不足。 |
| 没有可用价格 | **200** `pricing_status=unavailable/amount_usd=null` | 报价失败，不能预估为 0 或继续收费执行。视频可能另有 diagnostic 解释配置读取失败。 |

视频报价 response 的 `diagnostic` 是**顶层可选字符串**，不是 `error`；没有此字段不代表价格可用。SEP 当前 `PricingSchema.strict()` 必须显式接收该字段或采用允许扩展的策略，同时保留金额、规格、有效期等原有校验。

### A06/A10 单请求账务查询

参数无效返回 400，读取错误通常为 503，公共 Key/分组错误仍可能先发生。成功响应的状态只有现有的 `pending/settled`，没有因此新增“退款已完成”或“确认未提交”状态。

```json
{
  "object": "sub2api.request_usage",
  "schema_version": 1,
  "currency": "USD",
  "request_id": "client:example-correlation-id",
  "status": "pending",
  "actual_cost_usd": null,
  "pending_reason": "not_recorded_or_ambiguous"
}
```

图片 pending_reason 还可能是 `usage_recorded_billing_unconfirmed`、`transaction_confirmed_image_amount_unverified`。settled 时 pending_reason 为空字符串。

视频为 `object=sub2api.video_request_usage`，另有可空的 `model/video_count/duration_seconds/resolution`，当前没有图片同名的 pending_reason 承诺。SEP 按接口各自校验，不将两个 Schema 混用。

**生成结果、提交是否受理、账务是否结算是三件事。pending 不证明失败或未扣费；生成 failed 也不能单独成为 SEP 释放资金的充分证据。**

### A07/A08 图片生成与编辑

失败包含公共鉴权、参数/文件/大小校验、权限、限流、账号选择和上游返回。上游业务 code 保留原值，不承诺都是预定义平台枚举。SEP 请求的是 base64 输出时，校验 `data[].b64_json`、解码与文件有效性；支持 URL 的调用方另行校验 URL 输出。

错误返回可能是普通非 2xx JSON，也可能在图片 JSON 保活先提交 200 后以 JSON 正文返回 error。因此需要在解析成功图片 Schema 之前，先识别错误信封并保存实际原因；不要仅将其归为“结果格式不正确”后丢掉原错误。

HTTP 200 的 `{"error":...}` 在本轮修复涉及的普通图片路径会被识别为失败；不能据此推断所有供应商或尚未修复的转换路径都没有 200 业务失败。

### A11 视频创建

文生视频核心示例：

```http
POST /v1/videos
Authorization: Bearer <API_KEY>
Content-Type: application/json
X-Client-Request-ID: <SEP_REQUEST_ID>
Idempotency-Key: <SEP_REQUEST_ID>
X-Video-Pricing-Version: <PRICE_VERSION>
X-Video-Operation: text-to-video
```

```json
{
  "model": "seedance-2.0",
  "prompt": "草叶随风轻轻摆动。",
  "seconds": 4,
  "resolution": "720p",
  "aspect_ratio": "16:9",
  "generate_audio": false,
  "output_format": "video/mp4"
}
```

图生视频改为 multipart，重复 `input_reference` 上传图片，其他参数为表单字符串，operation 为 image-to-video。参考图支持 PNG、JPEG、WebP、GIF、BMP 和 TIFF；后端按图片内容识别格式并保留原格式供上游读取，不依赖文件名后缀或上传的 Content-Type。单张最多 10 MiB，宽高各最多 4096、像素总数最多 16,777,216；每请求最多 9 张，图片总量最多 90 MiB。当前 SEP V3 适配器已经不转发 fps；脑生云旧协议忽略未列入适配字段的参数，保留字段类型错误、output_format 不支持等仍会被拒绝，不等于无条件忽略所有无效参数。

| HTTP | `error.code/type` 或 `platform_code` | 含义与处理 |
|---|---|---|
| 400 | `invalid_request_error` | 缺 model/prompt/resolution、重复字段、类型错误、图数量不对等；保留包含具体字段的 message。 |
| 413 | `invalid_request_error` | 文件/请求体过大，按明确字节限制修正。 |
| 415 | `unsupported_encoding` | 压缩视频上传不支持。 |
| 422 | `unsupported_parameter/unsupported_spec/unsupported_duration` | 本地协议适配不支持当前参数、模型或时长；也可能是供应商 code，结合 source/内容辨别。 |
| 409 且 `X-Video-Dispatch=rejected` | `invalid_video_pricing_headers` | 报价保护头缺失、重复或格式错误，尚未派发。 |
| 409 且同上标记 | `video_pricing_changed` | 报价失效/规格等保护不匹配；message 给出具体原因，重新报价后按业务流程确认。 |
| 409 且同上标记 | `video_pricing_verification_failed` | 报价核验服务失败，不能直接断言价格变了。 |
| 409 且同上标记 | `video_pricing_account_mismatch` | 选定账号映射与报价不一致，需平台排障。 |
| 409 | `idempotency_conflict` | 相同幂等键用于不同参数，修正调用逻辑。 |
| 409 | `submission_unknown` | 原提交结果待确认；不能换幂等键直接再付费提交。 |
| 503 | `video_pricing_not_configured` | 生成所需价格没有配置。 |
| 503 | `video_reference_storage_error/video_reference_capacity_exceeded/video_upload_storage_error` | 参考图或暂存磁盘、容量、签名配置、Redis 等问题；message 指出阶段。 |
| 503 | `video_submission_unavailable/video_configuration_error/video_service_error` | 提交记录、配置或其他平台异常；保留原因，不能一概认定已派发或未派发。 |
| 429 | `rate_limit_error` 或供应商错误码 | 上传/执行并发、上游限流或配额问题，按 message 区分。 |
| 502/504 等 | `video_upstream_error/video_upstream_timeout/video_protocol_error`，或供应商 code + platform_code | 上游拒绝、网络超时、回执协议/身份不符等；可能已经发出请求，先按提交不确定处理。 |

旧协议生成约束：seedance-2.0、seedance-2.0-fast、seedance-2.0-mini 为 4..15 秒，seedance-2.5 为 4..30 秒；图生视频参考图 1..9 张、每张最多 10,485,760 字节，总计最多 94,371,840 字节，完整 multipart 最多 96,468,992 字节，非文件字段合计最多 2,097,152 字节；支持的图片格式为 PNG、JPEG、WebP、GIF、BMP、TIFF，宽高各 1..4096，像素总数最多 16,777,216。原生协议另有约束，不能套用旧协议的 9 张规则。

当前报价预派发拒绝示例：

```json
{
  "error": {
    "type": "invalid_request_error",
    "code": "invalid_video_pricing_headers",
    "source": "platform",
    "message": "Expected exactly one valid X-Video-Pricing-Version and X-Video-Operation (text-to-video or image-to-video) header"
  }
}
```

对应 HTTP 409 与 `X-Video-Dispatch: rejected`。SEP 当前只将其中 `video_pricing_changed` 判为 definitelyRejected，其余新增细分类别需由 SEP 评估并覆盖。不要将“任何 409”都作为可退款或可重提的依据。

### A12 视频任务查询

| 正文 status | 意义 | SEP 处理 |
|---|---|---|
| `queued` | 已排队 | 继续查询原任务。 |
| `in_progress` | 生成中 | 继续查询原任务。 |
| `completed` | 生成完成 | 下载并完成文件校验；账务独立核对。 |
| `failed` | 生成失败 | HTTP 仍可为 200；保存并展示 error，不继续按成功下载。 |
| `cancelled` | 已取消 | 作为任务终态；账务独立核对。 |

```json
{
  "object": "video",
  "id": "mtask_example",
  "model": "seedance-2.0",
  "status": "failed",
  "error": {
    "code": 1203,
    "message": "Reference image rejected",
    "type": "validation_error",
    "param": "images",
    "details": { "limit": 9 },
    "source": "upstream",
    "upstream_request_id": "example-provider-query-id"
  }
}
```

示例 task ID 只是占位，不能拿它发真实请求。实际使用创建接口返回的原 ID。

查询请求本身也可能非 2xx：400 ID 不合法；404 任务不存在/关联过期/不属于该 Key；503 账号或 Redis 关联不可用；旧协议供应商查询 404 可返回 404；其他上游拒绝可能映射 502。**一次查询失败不等于生成任务 failed**，保留“查询失败”与上次已知任务状态。

查询和下载绑定创建时的 Key。即使是同一个算力用户，换另一把 Key 也不能直接接管旧任务。当前视频关联 Redis 保留 7 天，过期不能根据 404 推断供应商从未接单。

### A13 视频下载

| 情况 | 对外表现 | SEP 处理 |
|---|---|---|
| 任务未完成 | 旧协议 409 `video_not_downloadable` | 继续查原任务，不重新创建。 |
| 任务已失败 | 409；供应商 code/message 可保留，另有 `platform_code=video_not_downloadable/stage=video_generation` | 定位为生成失败导致不可下载，不能误报单纯文件网络错误。 |
| 任务完成但没有 video_url | 旧协议 503 `video_output_unavailable` | 保存原因，继续按原任务核对，不创建替代任务。 |
| 文件 URL 不安全 | 502 `video_download_url_rejected`；连接阶段发现不安全目标也可能包装为 `video_download_error` | 保存具体原因，交由平台检查文件源地址；下载失败不代表原生成任务失败。 |
| 文件格式/长度异常 | 502 `video_output_invalid` 等；已开始发送后的异常见断流项 | 保存具体原因，当前下载失败不代表原生成任务失败。 |
| 文件源返回错误 | 通常 502，保留源站 message/code/request ID/upstream_status；平台类别可在 platform_code | 可区分签名过期、权限、文件不存在等；原任务查询/下载重试与重新生成分开。 |
| 下载超时 | 504 `video_download_timeout` 等 | 保存超时阶段；按原任务重新下载，不重新生成。 |
| 已发出视频字节后断流 | 可能已是 HTTP 200，仅网络读取错误/长度不匹配，无新 JSON 正文 | 临时文件不能发布为成品；记录已收字节、长度、跟踪 ID和读取异常。 |

旧协议文件源首次 403/404 在未向 SEP 发文件前，算力平台已经最多刷新并重试一次；不要据此让 SEP 在一个请求里无限重试。旧协议不承诺 Range 断点续传，当前 SEP 下载完整文件。原生分支可透传 Range/条件响应，与 SEP 当前只接受完整 200 的约束应分开。

## 6. SSE 与连接错误

HTTP 200 仅说明响应头已提交，不说明流成功。等待槽位的 ping、保活或部分文本输出之后，无法再把响应状态改为 502。

Chat 类错误示例：

```text
event: error
data: {"error":{"message":"Provider balance exhausted","code":"quota_exhausted","type":"permission_error","source":"upstream","upstream_status":403}}

```

Messages 风格还可能没有 event 行，只在 data 中携带 `{"type":"error","error":{...}}`。供应商直接流式返回的错误事件可能有自身结构，不能只识别 event 名称。

Responses 附录接口的终止失败示例：

```text
event: response.failed
data: {"type":"response.failed","response":{"id":"resp_example","object":"response","status":"failed","output":[],"error":{"message":"Unsupported size","code":"bad_size","param":"size","source":"upstream","upstream_status":422}}}

```

SEP 应同时识别 `error`、`type=error`、`type=response.failed`/`response.error` 等对应协议失败；收到错误后不要因后续 EOF、[DONE] 或已存在部分文字就覆盖为成功。SSE 跨 chunk、多行 data 要完整组装后解析；不是每个网络 chunk 都是一条 JSON。

没有错误事件但异常断开、未收到对应协议的有效结束条件时，记录为“连接中断/结果不完整”，保留最后已知输出与读取异常；不要编造供应商错误码或将本地 Abort/解析失败冒充供应商明确拒绝。

**仍未修复的算力平台边界**：本轮按要求暂缓 Anthropic、Gemini 部分转换丢失流内业务错误的问题。若该分支已经把错误变成正常完成，SEP 不能从不存在的字段恢复真实原因；本文不承诺所有转换事件已完整保留。

## 7. SEP 本次需要重点对接的位置

这些是对当前本地 SEP 源码的核对结果与建议，没有实施修改。

| SEP 文件/位置 | 当前处理 | 本次建议 |
|---|---|---|
| `backend/src/common/errors/media-upstream.exception.ts:34` | 媒体失败保留 JSON/纯文本 body，最大读取 64 KiB | 保留这项能力；错误正文读取异常/超限时也保留原 HTTP 状态和头，再附加本地读取失败，不仅剩读取异常。 |
| `backend/src/modules/media-gateway/sub2api-image.adapter.ts:195` | 非 2xx 可保存完整媒体错误；2xx 随后按图片成功 Schema 校验 | 加入 2xx 错误信封识别，避免已返回具体 error 却只剩 ImageDispatchError 的默认文案。保存诊断响应头。 |
| `backend/src/modules/media-gateway/sub2api-video.adapter.ts:23` | 视频报价 Schema 使用 strict | 接收可选顶层 diagnostic，保留原有严格金额和规格校验。 |
| `backend/src/modules/media-gateway/sub2api-video.adapter.ts:132` | 视频创建保存任务与请求关联；失败按 DispatchError 管理 | 保留真实 body，兼容数字错误码和新报价保护分类；诊断、是否派发、账务状态分别记录。 |
| `backend/src/modules/media-gateway/sub2api-video.adapter.ts:165` | 查询任务 error 为任意 JSON，已有保留能力 | 继续保留；任务 failed 与查询请求失败分开，支持展示 code/message/details。 |
| `backend/src/modules/media-gateway/sub2api-video.adapter.ts:176` | 下载读取设上限与完整性检查，非 2xx 使用媒体错误读取器 | 继续保留；断流记录请求头与读取原因，失败暂存文件不发布，重试原下载。 |
| `backend/src/modules/gateway/gateway.service.ts:208` | 仅从 body.error 取少量字段；code 只接收字符串；message 截到 1000；429 一律加 source=upstream | 兼容公共顶层错误；保留数字 code、details、真实 source/upstream_status/请求 ID/attempts；不要按 429 擅自改写来源。 |
| `backend/src/modules/gateway/gateway-stream.ts:155` | SSE observer 能标记 json.error，但没有保存完整错误诊断 | 在错误回执/日志中保存事件诊断，不能只留 protocolError 或 UPSTREAM_STREAM_ERROR。 |
| `backend/src/modules/model/model.service.ts:89` | 模型同步非 2xx 主要提示“检查密钥” | 按实际正文区分权限、存储、限流等；接收 partial/diagnostics，防止将降级目录当权威全量。 |
| `backend/src/modules/model/model-billing.service.ts:22` | 失败统一为 MODEL_BILLING_UNAVAILABLE | 对外业务状态可保留，同时记录算力原始状态、脱敏原因与请求 ID，不只剩通用文案。 |
| `backend/src/modules/knowledge/embedding.service.ts:112` | 非 2xx 主要保留 HTTP，部分异常改固定文案 | 读取并保存算力返回的具体错误，区分 transport、权限、模型、规格和输出校验。 |
| `backend/src/modules/conversation/conversation-stream.service.ts:390` 及数字员工 SDK 调用 | SDK 封装模型请求 | 核对 SDK 异常/事件能否保留 response body、header 和 cause；不能只取 SDK 的泛化 error.message。 |
| 任务规划/交付、文案、洞察、模型测试等直接 fetch 调用点 | 各自有异常包装 | 分别接入相同诊断保留规则，避免只修媒体链路。 |

### 7.1 建议 SEP 保存的诊断记录

| 建议字段 | 内容 |
|---|---|
| `origin` | longdao 或 SEP 本地，区别于错误体原有 source。 |
| `operation/endpoint/method` | 例如 video.create、video.query、video.download，记录不含凭证的接口路径。 |
| `httpStatus` | 本次收到的算力 HTTP 状态；网络失败没有响应时为空，不伪造供应商 HTTP。 |
| `body` | 有界脱敏后的原始 JSON 值或纯文本，保留数字、数组及扩展字段。 |
| `message/code/type/param/details` | 用于检索和显示的提取字段；提取失败不能因此删除 body。 |
| `source/upstreamStatus/platformCode` | 从错误体提取的来源、供应商状态和平台包装分类。 |
| `gatewayRequestId/clientCorrelationId/upstreamRequestId` | 分别保存响应的各类跟踪 ID；不要存成同一字段不断覆盖。 |
| `taskId` | 生成任务 ID，与跟踪 ID 分开。 |
| `retryAfter/attempts` | 上游提示和失败尝试诊断。 |
| `submissionState/taskStatus/billingState` | SEP 原有状态独立保存，本说明不要求更改它们的状态机。 |
| `readError/truncated` | 解析、读取、超限、截断等本地处理异常，附在收到的诊断之外。 |

建议原因提取顺序：协议明确的任务/事件 error 对象 → `body.error.message` → 字符串 `body.error` → 顶层 `message/detail` → JSON 字符串/纯文本 → “未返回可读原因”加本地阶段说明。保留原 body，不能仅保存提取出的字符串。若 error 是结构对象且 message 是数组/对象，保留原结构，并生成安全文本摘要。

禁止记录请求 Authorization、Cookie、真实 Key、完整签名 URL 等凭证。算力已经脱敏不代替 SEP 对本地异常/URL 的保护；但不能以脱敏为由删除整条业务原因。

## 8. 建议 SEP 验收样例

不需要调用收费模型，可用模拟算力响应覆盖：

| 场景 | 验收结果 |
|---|---|
| 401 顶层 INVALID_API_KEY | 能显示 Invalid API key 并保留码，不变成“未知错误”。 |
| 502 + error.upstream_status=422 + bad_size/details | 看到具体规格原因，保留两个 HTTP 层次与 details。 |
| 429 余额/额度耗尽与 429 并发满 | 分类不同，保留 Retry-After，不擅自将 source 改成供应商。 |
| 200 error，模拟图片保活后的失败 | 识别为图片失败，原始原因仍在，不误存成功或只报 Schema 不符合。 |
| 200 pricing_status=unavailable + diagnostic | 报价不可用，diagnostic 可读取，不因可选字段拒绝整个响应。 |
| 200 partial=true 的模型目录 | 标为降级，避免直接按全量结果清除配置。 |
| 200 视频 status=failed、error.code=1203 | failed 终态和数字 code/message/details 均保留。 |
| 视频查询 503 | 记为查询失败，原任务状态不被凭空改为 failed。 |
| 409 X-Video-Dispatch=rejected 的四种报价保护码 | 能区分格式、价格、核验、账号映射，按明确拒绝证据处理。 |
| 409 submission_unknown | 查询原请求，不换 ID 自动创建第二笔。 |
| SSE HTTP 200 后 error/response.failed | 保留原因，不被 EOF/[DONE]/部分文本覆盖为成功。 |
| 200 MP4 下载到一半断开 | 暂存成品不发布，记录字节和异常；重试原下载不重建任务。 |
| 错误为纯文本、code 为数字、未知扩展字段、正文读取异常 | 不因格式差异丢弃原状态/原因；本地读取失败独立记录。 |

以上是**建议验收清单**，不是声称本次已在 SEP 执行了这些测试。本次完成的是源码与路由核对及文档整理；算力修复阶段的模拟验证另见 `error-information-integrity-fixes-20261118.md`。

## 附录 A：算力平台还提供的兼容接口

下表用于防止后续 SEP 接入时遗漏。除与正文相同的别名外，未在当前 SEP 后端找到已调用证据，不要求 SEP 本轮无条件接入这些能力。未读取其他独立客户端运行配置，不据此断言所有外部调用都不存在。

| 方法与路径 | 返回/错误方式 |
|---|---|
| `POST /v1/messages` | Anthropic JSON/SSE；本地错误通常 type=error/error；公共鉴权仍可顶层 code/message。 |
| `POST /v1/messages/count_tokens` | token 统计 JSON，本地校验、估算或上游错误；分组不支持时 404。 |
| `POST /v1/responses`、`POST /v1/responses/*subpath` | Responses JSON/SSE；错误除普通 JSON 外可为 response.failed；通配路由不证明任意子功能可用。 |
| `GET /v1/responses` | WebSocket；升级前 HTTP 错误，升级后可能 error 帧和关闭事件；仅监听 close 会遗漏诊断。 |
| `POST /v1/alpha/search` | 搜索 JSON，含 Responses 后备调用；失败保留错误，不能将空 output 当正常完成。 |
| `GET /v1/usage` | 聚合用量 JSON；可能 200 部分数据，partial/diagnostics 标记读取错误。 |
| `POST /v1/images/generations/async`、`POST /v1/images/edits/async` | 202 受理与 poll_url/Location/Retry-After；仅受理不代表生成成功。 |
| `GET /v1/images/tasks/{task_id}` | 200 processing/completed/failed；failed 时读取 error/http_status；外层 200 是查询成功。错误归属可为 403 IMAGE_TASK_FORBIDDEN，404 IMAGE_TASK_NOT_FOUND，503 IMAGE_TASK_UNAVAILABLE。任务默认保留 24 小时，不与视频 7 天混用。 |
| `POST /v1/images/batches`、`GET /v1/images/batches`、`GET /v1/images/batches/models` | 批图片创建、列表与目录；HTTP 错误含 error，来源可为平台或上游。 |
| `GET /v1/images/batches/{id}`、`GET /v1/images/batches/{id}/items` | 任务级 error.code/message 与单项 error；外层查询 200 不代表每个项目成功。 |
| `GET /v1/images/batches/{id}/items/{custom_id}/content`、`GET /v1/images/batches/{id}/download` | 单项文件或 ZIP；开始前 JSON 错误，开始后复制失败主要通过断流和日志识别。 |
| `POST /v1/images/batches/{id}/cancel`、`DELETE /v1/images/batches/{id}`、`DELETE /v1/images/batches/{id}/outputs` | 任务控制/删除，错误使用批处理 error；记录与输出文件删除是不同操作。 |
| `POST /v1/videos/generations`、`POST /v1/videos/edits`、`POST /v1/videos/extensions` | Grok 专用视频生成/编辑/延长接口，不能当作当前 Seedance POST /videos 的等价入口。 |
| `POST /v1/video/generations` | 当前 OpenAI Videos handler 的创建别名，可有正文所述视频错误。 |
| `GET /v1beta/models`、`GET /v1beta/models/{model}`、`POST /v1beta/models/*modelAction` | Gemini 原生目录及 generateContent/streamGenerateContent 等实际支持的 action；Google 风格错误和 SSE，不能把通配注册解释为任意 action 都实现。 |
| `GET /antigravity/models`，`/antigravity/v1` 下的 messages/count_tokens/models/usage，`/antigravity/v1beta` 下的 models 与模型 action | 强制 Antigravity 调度的专用入口，沿对应协议错误格式。 |
| 无 /v1 前缀的 responses、responses 子路径、alpha/search、models、chat/completions、embeddings、图片同步/异步及 tasks、视频系列对应路由 | 已注册别名，仍按具体方法匹配；并非所有 /v1/sub2api 或批量接口都存在无前缀别名。 |
| `/backend-api/codex/responses` 及子路径、`GET /backend-api/codex/responses`、`POST /backend-api/codex/alpha/search`、`GET /backend-api/codex/models` | Codex 专用别名/目录；models 也可能通过 GET /v1/models?client_version=... 进入另一种 manifest 响应，不能套用普通 data[] Schema。 |
| `/media/video-inputs/{filename}` | 参考图签名 URL，由脑生云下载，不是 SEP 创建视频时另行调用的上传接口。可返回纯文本 403 无效签名、410 过期、404 不存在、429 下载并发、503 存储问题。签名 URL 不应放入公开错误或普通日志。 |

## 附录 B：主要核对依据

文件路径以各自项目根目录为基准，行号对应本地整理时版本。

| 项目与位置 | 依据 |
|---|---|
| LongdaoAI `backend/internal/server/routes/gateway.go:116` | 模型、报价、账务、图片、视频及兼容入口注册。 |
| LongdaoAI `backend/internal/server/middleware/middleware.go:64`、`api_key_auth.go:34` | 公共顶层错误结构与鉴权错误分支。 |
| LongdaoAI `backend/internal/server/middleware/client_request_id.go:20` | 客户关联 ID 生成与响应头。 |
| LongdaoAI `backend/internal/service/upstream_error_diagnostic.go:152` | 诊断字段合并和失败尝试。 |
| LongdaoAI `backend/internal/handler/gateway_image_billing.go:63`、`:141` | 图片报价与单请求账务。 |
| LongdaoAI `backend/internal/handler/gateway_video_billing.go:75`、`:152`、`:183` | 视频报价、预派发拒绝分类与账务。 |
| LongdaoAI `backend/internal/handler/openai_videos.go:22` | 视频入口、上传校验、任务归属、并发与价格检查。 |
| LongdaoAI `backend/internal/service/video_legacy.go:451`、`:505` | 失败任务正文、旧视频创建/查询错误与成功回执。 |
| LongdaoAI `backend/internal/service/video_download.go:118` | 成品下载、链接刷新与断流处理。 |
| LongdaoAI `backend/internal/handler/stream_error_event.go:56` | Responses 流内失败事件。 |
| SEP `backend/src/modules/media-gateway/sub2api-image.adapter.ts:88`、`:102`、`:124`、`:135` | 图片目录、报价、账务与同步生成/编辑实际调用。 |
| SEP `backend/src/modules/media-gateway/sub2api-video.adapter.ts:82`、`:96`、`:116`、`:132`、`:165`、`:176` | 视频目录、报价、账务、创建、查询及下载实际调用。 |
| SEP `backend/src/modules/gateway/gateway.service.ts:172` | 聊天请求与错误重包装。 |
| SEP `backend/src/modules/model/model-billing.service.ts:22` | 倍率读取。 |
| SEP `backend/src/modules/knowledge/embedding.service.ts:112` | 条件启用的 Embeddings 调用。 |

本次未核实：实际生产路由版本、网关反向代理产生的额外错误、浏览器 CORS 对诊断头的可见性、SDK 的所有运行时错误格式以及真实供应商返回。需要上线联调时再逐项确认，不能由本地文档替代。
