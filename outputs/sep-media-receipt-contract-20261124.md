# SEP -> Longdao 图片、视频完整回执

本次改动在本地 main，尚未部署。本文说明 Longdao 对 SEP 的回执，不是脑生云原生协议，也不要求 SEP 修改传给脑生云的参数。

## 1. 适用接口

| SEP -> Longdao | 用途 / operation |
|---|---|
| POST /v1/images/generations、/v1/images/edits | 同步生图 / create |
| POST /v1/images/generations/async、/v1/images/edits/async | 异步生图 / create |
| GET /v1/images/tasks/{id} | 图片任务查询 / query |
| GET /v1/images/tasks/{request_id}?id_type=request_id | 按原请求标识恢复回执 / query |
| GET /v1/images/tasks/{id}/content | 图片下载 / download |
| POST /v1/videos、/v1/video/generations | 视频创建 / create |
| POST /v1/videos/references | 上传参考视频 / upload，不创建生成任务、不计费 |
| GET /v1/videos/{id} | 视频任务查询 / query |
| GET /v1/videos/{request_id}?id_type=request_id | 按原请求标识恢复回执 / query |
| PUT /v1/videos/{id}/cancel | 视频取消 / cancel |
| GET /v1/videos/{id}/content | 视频下载 / download |
| POST /v1/sub2api/images/pricing、/v1/sub2api/videos/pricing | 本地报价 / pricing |
| GET /v1/sub2api/usage?request_id={id} | 图片用量 / usage |
| GET /v1/sub2api/videos/usage?request_id={id} | 视频用量 / usage |

已有不带 /v1 的生成、任务、下载、取消别名同样接入前置错误回执。Batch Images 和聊天协议不在本次改动范围。

## 2. 固定字段

所有这些接口的 JSON 错误均补齐以下字段，不以 HTTP 400、401、403、409、413、422、429、500、502、503、504 直接推断任务或扣费结果。成功的任务回执和报价、用量响应也补齐元信息，并保留原业务字段。

| 字段 | 含义 |
|---|---|
| schema_version | 2 |
| request_id | 原创建请求的 X-Client-Request-ID；未查到或未通过鉴权时只返回当前调用的关联编号 |
| task_id | Longdao 可查询的任务 ID；不存在或尚未授权读取时为 null。异步图片是本地任务 ID，不等同于脑生云 jobid |
| operation | create / query / cancel / download / pricing / usage / upload |
| receipt_operation | 本次接口操作，取值同上；报价接口原有 operation 仍保留 generate/edit/text-to-video/image-to-video 的业务含义，使用此字段识别 pricing |
| submission_status | 创建是否发出、是否被接受，见下表 |
| status | 已观察到的生成任务状态；没有任务证据时 null。用量接口保留既有顶层账务 status，不应当作任务状态 |
| is_final | 任务是否终态；未确认任务状态时 null |
| status_observed_at | 状态观察时间，Unix 毫秒；缺少可靠观察时间时 null |
| status_source | upstream / cached；没有任务观察时 null |
| task_error | 已确认任务自身的失败原因；查询失败不会被写入此字段 |
| error | 本次失败的错误诊断；任务回执兼容字段可能仍包含原任务错误，应优先结合 operation_error、task_error 判断 |
| operation_error | 本次 HTTP 操作失败原因；例如查询 503、取消 409、下载失败。成功的操作为 null |
| submission_error | 原创建操作失败原因，便于后续查询成功时仍获得历史失败诊断 |
| billing.status | not_charged / pending / settled / unknown / not_applicable |
| billing.actual_cost_usd | USD 字符串，明确不扣费为 "0"，尚未确认金额为 null |
| billing.error | 账务异常或证据缺失原因；与上游生成错误分开 |
| submission_retry | 视频已有的 attempts、max_attempts、exhausted、errors；不适用时 null |
| submission_record_error | 无法保存失败回执时的具体存储错误；不表示上游生成错误 |
| http_status | 失败创建记录中保留的原创建 HTTP 状态；后续 GET 查询成功时它仍可能是 502，不是当前 GET 的 HTTP 状态 |

| submission_status | 含义 |
|---|---|
| not_submitted | 鉴权、参数、报价、并发、配置等本地检查阻止了创建，本次调用不扣费 |
| rejected | 上游明确拒绝创建；不据其状态码推断用户已被扣费 |
| failed | 创建未取得可用回执或重试耗尽，按平台策略关闭且不向用户扣费 |
| accepted | 有已接受的任务/生成结果；任务状态和账务分别读取对应字段 |
| submitting | 已有创建过程正在进行，不能重复创建 |
| protocol_error | 收到任务 ID，但回执状态、模型或结构不符合协议；保留 ID 供诊断，不再次创建，按平台策略关闭用户扣费 |
| unobserved | 这次查询/下载/取消未取得有权限的原任务证据；不等于原创建失败 |
| not_applicable | 报价、参考视频上传等不创建任务的操作 |

异步图片已经存在的本地 submitting / failed / accepted 状态保持兼容。原生视频存在历史账务数据时仍以账务证据为准。unknown 账务结果必须携带原因，不能被当作供应商已收费或已退款。

## 3. 具体错误保留

error、operation_error 保留供应商可提供的 message、code、type、param、reason、details、upstream_status、upstream_request_id、retry_after 等诊断字段。缺少 code/type 时只补平台诊断标识，不替换具体 message。凭证、签名、Cookie、个人信息及内部诊断敏感片段按已有脱敏器处理。

Longdao 自己的鉴权、参数校验、连接失败、协议错误使用 platform/transport 来源或原有平台错误来源，不伪造为脑生云业务拒绝。HTTP 502 仍可保留，正文同时保留实际供应商状态和原因。

示例：上游明确没有可用图片账号，同步生图失败：

```json
{
  "schema_version": 2,
  "request_id": "sep-request-id",
  "task_id": null,
  "operation": "create",
  "submission_status": "rejected",
  "status": null,
  "is_final": null,
  "status_observed_at": null,
  "status_source": null,
  "task_error": null,
  "submission_retry": null,
  "billing": {"status":"not_charged","actual_cost_usd":"0","error":null},
  "error": {
    "source":"upstream",
    "type":"api_error",
    "code":"media_operation_error",
    "message":"No available accounts for image generation",
    "upstream_status":503
  }
}
```

示例省略重复的 operation_error、submission_error；实际响应会提供。供应商有 code 时保留原值，不强制使用示例 code。

## 4. 账务和恢复规则

1. 已鉴权且没有可计费输出的失败创建回执，按用户 ID、分组 ID、原 Key ID、媒体种类、原请求 ID 保存 Redis 7 天。无图片、提示词和凭证副本。
2. 相同 Key 重用已有失败请求 ID 时返回原失败回执，不发起新的付费请求。重新生成须用新请求 ID。原有异步任务、幂等重放和任务绑定机制继续生效。
3. 用量查询先读取已有数据库账务记录及用量扣费证据；只有没有这些证据时才使用失败回执确认 not_charged。不会用 Redis 失败记录覆盖已有扣费事实。
4. 图片/视频按 request_id 查询时可以恢复只有本地拒绝、没有上游任务 ID 的失败回执。原任务存在时优先读取原任务链路。
5. 换另一把 Key 无权恢复原 Key 的回执，即使属于同一个用户。鉴权失败不查询和泄露原任务。
6. 早于本版本、已经过期或根本未记录的请求，不能因查不到记录就声称不扣费。保留账务未确认及具体原因。
7. 同步图片响应写出时，后置用量入账可能尚未完成，因此成功生成可返回 billing.pending；用量查询以确认的账务证据返回 settled。部分图片已生成后发生错误同样不能声称不扣费。
8. unknown 账务表示 Longdao 尚未拿到可确认的本地财务证据，不代表脑生云任务创建 unknown，也不代表供应商账单应由用户承担。

参考视频上传成功（HTTP 201）和失败均返回 operation=upload、submission_status=not_applicable、task_id=null、billing.status=not_charged、billing.actual_cost_usd="0"。成功保留 reference_id、content_type、size_bytes、expires_at；失败通过 error/operation_error 返回上传原因。上传不写入、不读取视频创建失败重放缓存，因此上传和生成即使使用同一请求编号也不会互相替代响应。存储错误保留配置、磁盘、Redis 或容量的具体原因，敏感路径与凭证按片段脱敏，容量错误保留 video_reference_capacity_exceeded 及实际字节数。参考视频的 7 天新任务使用期、最长 14 天文件保存期不在本次修复中调整。

## 5. 任务查询、取消和下载

- 脑生云查询接口的 401/403/404/500/503 是本次查询失败。能加载原任务时，保持其已知状态、原请求 ID 和账务，不把它们改成任务失败。
- 取消接口 409 是本次取消未成功，保持原任务状态。取消未成功不能直接声称不扣费。
- 下载前失败返回完整 JSON 操作错误，并保留已确认的生成和结算状态。生成成功但下载失败不是生成失败。
- 已开始的图片/视频二进制传输保持原格式，不能在文件尾部追加 JSON。传输中断由连接和已有日志体现，后续任务/用量查询恢复结果。
- 已开始的图片 SSE 保留 HTTP 状态；Longdao 生成的 error 事件补齐回执字段，不能在 HTTP 200 已提交后再改写 HTTP 状态。
- 同步图片 JSON 心跳已经提交 HTTP 200 时，后续错误以完整 JSON 正文表达，SEP 必须读取 error/operation_error，不能只判断 HTTP 状态。
- 正常图片 Base64 和文件不会被公共错误中间件整体缓冲。错误正文最多缓冲 256 KiB；超出时明确 diagnostic_truncated=true，禁止悄悄当完整诊断。

## 6. 上游协议依据

Longdao -> 脑生云：旧版视频创建 /v1/video/tasks 支持 Idempotency-Key，查询 /v1/video/tasks/{id}，取消 /v1/video/tasks/{id}/cancel。三种操作的失败意义分别处理。

GPT 异步图片使用创建 /v1/images/generations/async、查询 /v1/images/jobs/{jobid}；目前该文档没有承诺创建幂等重试，因此不新增图片自动付费重试。

参考文档标识：499628413e0（视频创建）、499628414e0（视频查询）、499628415e0（视频取消）、500889114e0（GPT 异步图片创建）、500889115e0（GPT 异步图片查询）。用户不扣费结论是 Longdao 的本地策略及账务事实，不能据此宣称供应商账单为零。
