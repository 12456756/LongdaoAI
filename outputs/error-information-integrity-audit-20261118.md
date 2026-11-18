# LongdaoAI 错误信息完整性审计

审计日期：2026-11-18。工作目录：`D:\github\LongdaoAI`。基线：本地 `main`，提交 `8fea90da06552b16589562aebc409227a2a6201d`。

本次只读检查项目代码并运行本地测试；仅新增本报告，没有修改源码、配置、重试、渠道切换、计费或任务状态逻辑。没有连接生产服务器、读取真实密钥、调用收费模型或修改生产数据。

结论：供应商错误与最终返回给 SEP 的错误并不总是一致。已复现固定文案替换、错误字段丢失、状态码改变、原始 cause 丢弃，以及部分协议转换将失败变成正常完成。不能把这些问题都归为“合理脱敏”。同时存在文本脱敏不足，不能直接用“所有原始响应无条件透传”解决。

证据级别：**模拟复现**指调用当前实现、输入人工构造的上游响应；**代码确认**指已追踪注册路由到执行分支；**未验证**指配置、真实供应商或运行时条件尚未确认。模拟结果不是生产事故认定。

## 用户确认的后续范围（2026-11-18）

- 暂不处理Anthropic、Gemini部分协议转换丢弃流内错误的问题。下文保留原审计事实，不将这些分支纳入当前拟修复范围。
- 保留现有HTTP状态码映射，包括供应商HTTP422转换为平台HTTP502；不以恢复供应商HTTP状态码为本轮后续目标。
- HTTP状态码映射与错误内容保留分别处理：后续仍应保留脱敏后的真实message、有诊断价值的业务code/type/param/details及供应商请求ID；平台返回502不能成为删除原始原因的依据。
- 其余审计问题作为后续完善方向，包括旧视频错误解析、下载诊断、重试/渠道切换后的原因保留、批量/异步错误信息、本地错误归类及敏感片段脱敏；不改变既有重试、渠道切换、计费或任务状态策略。
- 当前仍处于审计与范围确认阶段，没有实施源码修改。第8节是原始审计建议，实际后续范围以上述取舍为准。

归因说明：供应商返回422只能证明供应商拒绝了收到的请求，单凭该状态不能确定是SEP参数、算力平台转换还是供应商约束导致。保留实际错误内容和上游状态的内部记录，有助于后续判断责任来源；对SEP的HTTP状态映射继续保留。

## 1. 主要问题，按影响排序

### F01 / 高：协议转换可以把上游失败转换成 HTTP 200 正常完成

- 实际入口：非 OpenAI/Grok 分组的 `/v1/responses`、`/v1/chat/completions`，分派位置分别为 `backend/internal/handler/gateway_handler_responses.go:226`、`backend/internal/handler/gateway_handler_chat_completions.go:253`。
- Anthropic SSE 转 Responses/Chat：事件解析未处理供应商 `error`；收到 `message_start` 后接 `event: error`，缓冲路径仍合成正常响应，流式路径仍合成完成事件。位置：`backend/internal/service/gateway_forward_as_responses.go:228`、`:368`，`backend/internal/service/gateway_forward_as_chat_completions.go:221`、`:353`。事件结构 `backend/internal/pkg/apicompat/types.go:148` 没有承载错误对象的字段。
- 扫描错误也可能只记录后继续成功收尾：Responses `:311`、`:503`；Chat `:294`、`:473`。缓冲 Responses 只要求曾收到初始响应，随后在 `:350` 写 200，没有要求正常终止。
- Gemini 转 Chat 流式：`backend/internal/service/gemini_chat_completions_compat_service.go:492` 忽略只有 `error` 的事件，最后输出 `finish_reason: stop` 和 `[DONE]`。Gemini 转 Messages 同类解析在 `backend/internal/service/gemini_messages_compat_service.go:1968`，未显式处理该业务错误事件，末尾仍发送正常结束事件。
- Alpha 搜索的 Responses 后备路径：`backend/internal/service/openai_alpha_search.go:512`、`:523` 只汇总文本与完成事件，未处理 `response.failed`。调用方 `:197` 后按成功结果继续。
- **模拟复现**：Anthropic 转换的缓冲/流式共 4 种路径、Gemini 转 Chat 流式、Alpha 失败事件解析均丢失原始原因。前两类方法返回 `nil error`，对外 200；Alpha 解析得到 `{"output":""}` 与 `nil error`。
- 可追溯性：被忽略的业务错误未进入这些转换方法的错误日志/上游错误上下文；扫描器读取错误部分有日志。不能指望 Ops 仅从 200 响应重建被删除的错误。handler 还可能进入成功用量处理路径，是否实际扣费取决于用量和计费逻辑，本次未验证实际账务。
- 影响：SEP 可能把失败任务当成功、保存空内容或停止重试。这比固定错误文案更严重。

### F02 / 高：文本主链路按状态码改写真实原因，甚至改写错误性质

- Anthropic 核心：`backend/internal/service/gateway_upstream_response.go:467`。400 是保留原正文的例外；401/403 改成 502 与固定认证/权限文案；429 固定限流文案；529 改 503；其他 5xx 改 502；例如 422 最终变 502 `Upstream request failed`。重试耗尽出口 `:626` 也是通用 502。
- OpenAI Responses 核心：`backend/internal/service/openai_gateway_upstream_errors.go:511`。401/402/403 改 502 与固定文案，429 改固定限流文案，其余默认 502。`:533` 上下文窗口错误保留 message，是例外。
- OpenAI 名为 passthrough 的路径也会改写：`backend/internal/service/openai_gateway_passthrough.go:522`。401/403 改 502；其他状态大多保留，但 message 固定；原 `code/type/param/details` 均不返回。
- 账号自定义状态码处理排除某状态时，OpenAI 核心 `backend/internal/service/openai_gateway_upstream_errors.go:449`、图片 `backend/internal/service/openai_images_responses.go:878` 会改为 500 `Upstream gateway error`。这不是供应商实际返回的 500。
- 管理员“错误透传规则”也不等于原始正文透传：`backend/internal/service/error_passthrough_runtime.go:59` 的 PassthroughBody 实际只提取message；`:70` 将type固定为upstream_error，其余字段没有承载。规则还可自定义状态/文案及跳过监控。已确认handler实际绑定该服务，但未读取线上规则，不能推测某次请求具体命中了哪条。
- Antigravity 转 Messages：`backend/internal/service/antigravity_gateway_streaming.go:691`，仅部分 400 保留 message；其他按状态换固定文案。Gemini 转 Messages 的映射器 `backend/internal/service/gemini_messages_compat_service.go:1876` 仅识别 Google 嵌套错误；不能解析时走 `:1770` 的固定文案，哪怕共享提取器已从顶层取到了 message。
- **模拟复现**：同一个 422、同一个 `bad_size` 完整错误，经 Anthropic/OpenAI 核心变成 502；经兼容 Chat/Messages 错误处理保留 422 和 message，但 code/param/details 丢失。
- 可追溯性：核心通常保留解析后的上游 message、状态和部分上游请求 ID 到 Ops 上下文；完整正文受配置与长度限制。部分服务端日志也有摘要。日志能否落库取决于 F10 条件。
- 影响：参数不合法被表现为服务异常，调用方可能反复重试，而不是修正参数；供应商余额、权限、配额问题可能被混为临时不可用。

### F03 / 高：重试/切换耗尽再次覆盖原因，部分流式出口直接返回

- `backend/internal/handler/failover_loop.go:81` 将最近失败保存到 `LastFailoverErr`，没有向客户端组织所有尝试的诊断结果。
- OpenAI 统一耗尽出口 `backend/internal/handler/openai_gateway_handler.go:2102`、`:2207`，以及 Anthropic `backend/internal/handler/gateway_handler.go:1453`、`:1502`：再次按最后状态生成固定文案。部分认证失败最终归为 503。原正文即使在 failover 对象中，仍可能不返回。
- 非 OpenAI 的 Responses/Chat 耗尽出口更直接：`backend/internal/handler/gateway_handler_responses.go:311`、`backend/internal/handler/gateway_handler_chat_completions.go:340`。流开始后直接 return；未开始时通常返回最后状态和 `All available accounts exhausted`。认证失败、silent refusal 等有独立例外。
- Gemini 原生耗尽出口 `backend/internal/handler/gemini_v1beta_handler.go:585`、`:627` 同样生成通用 Google 格式错误。
- 传输错误合成 failover：`backend/internal/service/openai_upstream_transport_error.go:108` 将实际错误写 Ops，上抛的 `:137` failover 正文则是合成通用错误，不能再从它恢复 typed cause。
- 搜索模拟是实际接入的内部路径：`backend/internal/service/gateway_forward.go:97`。搜索供应商失败虽逐次写日志，`backend/internal/pkg/websearch/manager.go:133` 最终仅返回 `no available provider (all exhausted or failed)`；`backend/internal/service/gateway_websearch_emulation.go:165` 又把代理错误放入纯文本 failover 正文，而共享 JSON 提取器不解析纯文本。
- 可追溯性：很多尝试已通过 `appendOpsUpstreamError` 单独保存，不能说“切换后所有服务端信息都丢了”；但客户端只剩最终概括，且 Ops 最多保存最后 32 个事件。搜索明细主要在普通日志。
- 影响：真实的第一处失败被“账号耗尽”掩盖；已经发送 200/SSE 头后直接返回的分支，客户端只看到流结束。该流式分支的实际触发条件未做整条路由模拟。

### F04 / 高：脑生云旧版视频错误解析过窄，部分原因在日志中也消失

- 实际分派：`backend/internal/service/openai_videos.go:249` 根据账号/已冻结任务协议进入 `forwardLegacyVideo`；不是所有 `/v1/videos` 都必经旧协议。
- `backend/internal/service/video_legacy.go:364` 的 `legacyErrorMessage` 只读顶层字符串 `message`。`{"error":{"message":"..."}}`、字符串 `error`、纯文本均不识别；code/type/param/details/供应商请求 ID 均未保存。
- 非预期 HTTP 状态在 `:331` 读取最多 65,536 字节，只记录上述提取结果。429 在 `:337` 固定为 `Video upstream is busy`；查询 404 在 `:340` 固定为 `Upstream video task unavailable`；其他状态改成 502，原 HTTP 状态只保留在 message 文本。
- `:320` 传输异常只返回固定 502/504；本函数不保存原始 err/cause，也没有记录具体传输错误。外层日志只能拿到已经改写后的错误。底层 HTTP 组件是否另有可用日志，本次不能保证。
- `:67`、`:74` 的成功状态正文校验，把业务非零 code/异常信封统一改为 `Invalid upstream video response`，没有附带原业务 code/message，也没有在这里记录原正文。
- **模拟复现**：嵌套 `error.message` 经过适配后只剩 `Video upstream returned HTTP 400`，对应日志 `reason=""`；顶层 message 可保留；429 原“配额耗尽”被改成“busy”；模拟 TLS 错误无法从返回 error/cause 恢复；业务非零 code/message 也被删除。
- 影响：同一视频接口取决于协议，诊断能力差异很大；配额耗尽可能被 SEP 当作短暂拥堵重试。

### F05 / 高：视频下载丢弃文件源错误正文，断流日志不记录具体错误

- 旧版下载在 `backend/internal/service/video_download.go:152` 将网络错误改成固定 502/504，未保留 cause。
- 文件源首次 403/404 在 `:159` 刷新链接再试；最终非 200 在 `:163` 直接关闭正文，最终 502 只含 `Video file source returned HTTP N`。源站的 `AccessDenied`、签名过期、对象不存在、请求 ID 等均丢失，没有先提取脱敏原因。
- 已开始 MP4 传输时，`:169` 收到具体 stream 错误后，`:171` 日志只写 task/account，没有 `err`；随后抛 `http.ErrAbortHandler`。此时不能改 HTTP 状态或在 MP4 中附加 JSON 是合理的，但服务端删除具体原因是不合理的。
- `backend/internal/handler/batch_image_handler.go:162` 的单图片下载和 `:196` 的 ZIP 下载也有中途失败直接 return 的分支，未在这些出口记录具体复制/ZIP 错误。
- 对比：原生视频下载 `backend/internal/service/openai_videos.go:368` 返回 `io.Copy` 的 error，外层 handler `backend/internal/handler/openai_videos.go:223` 记录，诊断保留情况较好。
- 影响：SEP 只能发现文件不完整或通用下载失败，无法区分源站权限、过期签名、网络中断、慢客户端。代码确认，未真实下载大文件或制造生产断流。

### F06 / 高：批量图片适配先丢 cause，再统一提交错误；任务级错误未公开

- Gemini API 错误读取器 `backend/internal/service/batch_image_provider_gemini.go:701` 本来能得到 message/code，但 `:433` 的映射器换成固定文案，并传 `nil` cause；非 typed 错误也在 `:450` 被丢弃。Vertex 类似映射在 `backend/internal/service/batch_image_provider_vertex.go:635`，其未分类异常保留 cause 的分支是例外。
- 提交服务 `backend/internal/service/batch_image_public.go:351`、`:1324` 再将大多数失败改成统一提交失败。失败记录拿到的也可能已经是改写后的 error。`:355` 释放预留失败还可优先返回释放错误，遮盖原提交错误。
- 批任务公共 DTO `backend/internal/service/batch_image_public.go:109` 没有任务级 ErrorCode/ErrorMessage；单项 DTO `:129` 有 `error.code/message/source`。因此只查任务可知 failed，却未必知道为何 failed，需要继续查 items，而且任务级错误不保证存在对应单项。
- 轮询 provider 失败 `backend/internal/service/batch_image_processor.go:108` 记录后重新排队，公开状态可能仍是上次状态，没有最后查询失败原因。
- `backend/internal/handler/batch_image_handler.go:265` 对所有错误固定 `type: invalid_request_error`，包括 500/502，构成错误归类。
- **模拟复现**：带真实模拟原因 `AUDIT_INVALID_IMAGE` 的 GeminiAPIError 转换后只剩 `Gemini API request failed`，原 cause 不可恢复。
- 可追溯性：任务/单项失败本来有数据库记录；但在 provider mapper 已删除的原因无法由后续持久化补回。本地队列、存储错误有各自业务 code，不应冒充供应商错误。功能是否在线启用未确认。

### F07 / 中高：异步图片仅按 HTTP 2xx + JSON 合法判成功，后台日志链路不完整

- `backend/internal/handler/image_task_handler.go:192` 对 2xx 只检查非空、JSON 合法，即调用 Complete；没有验证业务错误信封或确有图片输出。原生 API Key 图片路径 `backend/internal/service/openai_images.go:873` 也可把 HTTP 200 的错误正文原样写出并返回 nil。
- 因此若供应商用 200 返回 `{"error":...}`，在能够到达该路径的配置下，可能被保存为 completed；这是代码确认的组合风险，未做完整异步 handler 注入测试，不能宣称所有图片协议都如此。OAuth 图片另有流内错误识别，不受此简单结论覆盖。
- `backend/internal/handler/image_task_handler.go:247` 对 JSON 错误保留 RawMessage，做得较好；对非 JSON 则在 `:257` 改成固定 `image generation failed`。
- 异步执行通过 `:211` 复制 Gin context、`:182` 直接调用 handler；没有重新运行 Ops 中间件。提交请求原中间件处理的是 202，不保证能捕获后来在 taskCtx 新写入的诊断。已有 handler 普通日志仍可能保留部分原因，不等于完全无日志。
- 本地结果落对象存储失败由 `backend/internal/service/image_task.go:153` 后的逻辑记录日志并标记存储失败，必须区分“生成失败”与“生成后存储失败”。

### F08 / 中高：本地报价/计费错误混合归类，容易让 SEP 采取错误操作

- 报价接口本身读取本地价格与渠道配置，不是向供应商发报价 HTTP 请求。图片 `backend/internal/handler/gateway_image_billing.go:67` 对 JSON 语法、未知字段、超过 4,096 字节等，统一 400 `Invalid image pricing request`，不指出具体字段/解析原因。
- 图片 `:88` 将“分组未启用图片生成”“服务缺失”“模型不允许”合并为 403 `Model is not available for this key`，无法据此确认一定是模型未开放；视频 `backend/internal/handler/gateway_video_billing.go:71` 起的校验也有类似概括。
- `backend/internal/service/gateway_video_billing.go:78` 把 QuoteVideoBilling 的错误和无有效价格都改成 ErrVideoPricingChanged。配置/数据库读取失败可发生在 `:117`、`:127`、`:135`，部分底层 err 在此已不再保留。
- `backend/internal/handler/gateway_video_billing.go:140` 将报价版本/operation 格式、价格变化、账号不匹配、服务错误合并为 409 `video_pricing_changed`，并设置 `X-Video-Dispatch: rejected`。该 header 正确表示尚未派发，但 message 不能证明原因真是价格变化。
- 图片/视频单请求账务查询错误分别在 `backend/internal/handler/gateway_image_billing.go:143`、`backend/internal/handler/gateway_video_billing.go:167` 返回固定 503；这些出口不记录原始 err。
- 公共预检查 `backend/internal/handler/gateway_handler.go:1999` 保留已识别额度/RPM等错误；但未知 error 在 `:2034` 默认 403 `billing_error`。普通非 ApplicationError 经 `backend/internal/pkg/errors/errors.go:148` 变为 `internal error`，可能把平台内部故障表现为用户账务限制。
- 影响：SEP 可能无效地重复询价，或误以为用户无权限/余额不足。不能把这些本地产生的错误写成“供应商拒绝”。

### F09 / 中高：脱敏既有过度替换，也有敏感文本漏处理

- **过度替换**：`backend/internal/service/batch_image_public.go:1410` 只要 message 含 `gs://`、`files/`、`projects/` 就删除整条诊断，改成 `upstream provider operation failed`。模拟已复现。
- **过度范围**：`backend/internal/service/video_legacy.go:392` 替换所有 HTTP(S) URL，可能同时删除有诊断价值的接口路径、源站及资源定位。去掉凭证、签名和私有标识有必要，但整个 URL 无差别删除不等于仅移除敏感片段。
- **漏处理**：共享 `backend/internal/service/gemini_messages_compat_service.go:1695`、`:1700` 只遮蔽 URL 查询中的 `key/client_secret/access_token/refresh_token`。模拟字符串中的 Bearer、Cookie、含用户名密码的 DSN、邮箱原样保留。
- 规则路径 `error_passthrough_runtime.go:59` 提取message后未在该函数调用统一脱敏；不能因为启用了“透传”就假设已清除敏感片段，仍需逐个writer核对。
- Ops JSON 递归按敏感键名脱敏是合理基础：`backend/internal/service/ops_service.go:558`、`:581`；但字符串值不再扫描。`{"message":"Authorization: Bearer ..."}` 不会因 key 是 message 而脱敏。非 JSON 错误正文 `:788` 仅截断。
- Anthropic 核心 `backend/internal/service/gateway_upstream_response.go:364` 直接记录前 1,000 字节正文；`backend/internal/pkg/logger/logger.go:477` 仅规范空白/分级/写日志，没有统一文本脱敏；logger core 创建路径也未见自动秘密过滤。因此该调用点的敏感字段可能进入普通日志。是否有外部采集器二次脱敏未验证。
- 旧视频能精确替换本账号 key、创建请求的完整提示词，是已有合理保护；查询失败时没有原请求，提示词替换不一定适用。返回 Redis/文件错误的 `video_legacy.go:494`、`:550` 和 `video_reference.go:163` 等可把内部地址/路径带给客户端，需后续按片段处理。
- 这是代码层泄露风险，不表示本次发现了真实密钥泄露；本次所有验证只用虚构内容。

### F10 / 中：请求 ID、诊断头和运维记录不能稳定补回丢失原因

- 全局 `backend/internal/server/router.go:56` 接 RequestLogger；`backend/internal/server/middleware/request_logger.go:17` 建立并返回 `X-Request-ID`。它可能来自调用方或本地生成，不能自动当作供应商请求 ID。`client_request_id.go:19` 另处理 `X-Client-Request-ID`。
- 重建 JSON 错误通常只返回 message/type 等，没有复制供应商 `request_id` 或诊断头。图片 typed error 内有 `UpstreamRequestID`，但 `backend/internal/service/openai_images_responses.go:1014` 的 writer 没有写到响应体或头。
- passthrough 错误头策略 `backend/internal/service/openai_gateway_passthrough.go:492` 只选择性保留合法 Retry-After；通用核心/兼容错误并非完整头透传。默认成功/原样错误路径使用 `backend/internal/util/responseheaders/responseheaders.go:16` 的白名单，属于合理安全限制，但需单独保留诊断头。
- Ops 中间件 `backend/internal/handler/ops_error_logger.go:606` 主要捕获失败 HTTP 响应，200 需要显式上游上下文或 stream 标记。错误被转换为成功且未设上下文时，不会自动得到完整原始错误。
- OpenAI 通用流式错误 writer `backend/internal/handler/openai_gateway_handler.go:2225` 在该函数内未调用 MarkOpsStreamError；若是纯本地错误且没有别处记录上游上下文，Ops 可能漏记。对比 `backend/internal/handler/gateway_handler.go:1525` 已标记。需按调用点验证，不能将所有 OpenAI SSE 都说成无日志。
- `backend/internal/service/ops_service.go:298` 最多保留最后 32 个尝试，message 最多 2,048 字节、请求 ID 最多 128 字节；正文也有上限。`truncateString` 按字节截取并退到有效UTF-8边界，见 `backend/internal/service/ops_metrics_collector.go:928`。超长诊断不会完整保存。
- 监控开关 `ops_error_logger.go:629`、过滤规则 `:1691`、队列满丢弃 `:268`、写库失败都会影响追溯。规则还可主动跳过监控。未读取生产设置，不保证线上一定有这些记录。

### F11 / 中：模型目录/用量的降级结果没有说明数据读取曾失败

- `backend/internal/service/gateway_service.go:1181` 将账号仓库读取错误和无账号都返回 nil；普通 models handler `backend/internal/handler/gateway_handler.go:998` 起会使用默认目录。HTTP 200 模型列表不证明上游或当前实际账号支持这些模型。
- Gemini 原生模型目录的特定 scope 错误/部分模型 404 可触发静态后备目录，规则在 `backend/internal/handler/gemini_v1beta_handler.go:679`、`:698`。这是既有兼容降级，不能直接当作生成成功，但调用方看不到曾发生的供应商目录错误。
- `/v1/usage` 的统计错误在 `backend/internal/handler/gateway_handler.go:1154` 等被忽略，`:1194`、`:1230` 可返回 nil 数据，整体仍是 200，缺少“部分数据不可用”的明确标记。
- Codex 动态 models 不同：`backend/internal/service/openai_codex_models_service.go:441` 保留上游状态与截断正文到错误 message，handler `backend/internal/handler/openai_codex_models_handler.go:68` 返回；但 HTTP 通常已改成 502，结构字段仍未完整透传。

## 2. 实际接口清单

注册依据：`backend/internal/server/routes/gateway.go:116`。以下列出全部模型网关路由及配套异步、文件、报价、账务路由，不将管理后台的账号测试/配置 CRUD 视为 SEP 模型调用接口。没有在该路由集中发现独立 audio/rerank 接口。通配 Responses 子路径只有注册事实，不代表每个任意子路径都被供应商支持。

处理类型简称：T=文本/协议转换；I=同步图片；A=异步图片；B=批量图片；V=视频；M=模型目录；Q=本地报价/账务；R=本地参考文件。其具体错误、字段和头策略见第 3、4 节。

| 方法 | 实际注册路径 | 实际执行/错误处理 |
|---|---|---|
| POST | `/v1/messages` | OpenAI/Grok -> OpenAIGateway.Messages；其他 -> Gateway.Messages，再按实际账号进入 Anthropic/Gemini/Antigravity/Bedrock；T |
| POST | `/v1/messages/count_tokens` | OpenAI -> Responses input_tokens 兼容/后备；Grok 本地不支持；其他 -> Gateway.CountTokens，部分 Gemini 本地估算；T |
| POST | `/v1/responses`、`/v1/responses/*subpath` | OpenAI/Grok -> OpenAIGateway.Responses；其他 -> Gateway.Responses，包含 Anthropic 转换；T |
| GET | `/v1/responses` | WebSocket 入口，不等同 POST SSE；T/WS |
| POST | `/v1/chat/completions` | OpenAI/Grok -> OpenAIGateway.ChatCompletions；其他 -> Gateway.ChatCompletions，再按账号转换；T |
| POST | `/v1/alpha/search` | OpenAI AlphaSearch，含 Responses web_search 后备转换；T |
| POST | `/v1/embeddings` | 仅 OpenAI 分组支持的网关分派；普通响应可原样，切换失败走文本耗尽出口 |
| POST | `/v1/images/generations`、`/v1/images/edits` | OpenAI -> Images；Grok -> GrokImages；其他平台本地 404；I |
| POST | `/v1/images/generations/async`、`/v1/images/edits/async` | AsyncImage.Submit；后台调用同一个实际图片 handler；A |
| GET | `/v1/images/tasks/:task_id` | 本地归属校验及任务记录读取；A |
| POST、GET | `/v1/images/batches` | 提交/本地任务列表；B |
| GET | `/v1/images/batches/models` | 批量图片支持模型目录；B |
| GET | `/v1/images/batches/:id` | 任务查询；B |
| GET | `/v1/images/batches/:id/items` | 单项结果及单项错误；B |
| GET | `/v1/images/batches/:id/items/:custom_id/content` | 单项图片文件；B/下载 |
| GET | `/v1/images/batches/:id/download` | 输出 ZIP；B/下载 |
| POST | `/v1/images/batches/:id/cancel` | 本地/供应商取消；B |
| DELETE | `/v1/images/batches/:id`、`/v1/images/batches/:id/outputs` | 删除记录/输出；B，本地存储/清理错误也走批量错误 writer |
| POST | `/v1/videos`、`/v1/video/generations` | OpenAIGateway.Videos；再根据协议选择原生 `/v1/videos` 或脑生云旧 `/v1/video/tasks`；V |
| GET | `/v1/videos/:request_id` | OpenAI -> Videos（原生/旧版）；Grok -> GrokVideoStatus；V |
| GET | `/v1/videos/:request_id/content` | Videos 成品下载；原生代理或旧版 URL 下载；V/文件 |
| POST | `/v1/videos/generations` | 仅 Grok 视频生成分派，不能和旧版别名 `/v1/video/generations` 混淆；V |
| POST | `/v1/videos/edits`、`/v1/videos/extensions` | 仅 Grok 编辑/续写分派；V |
| GET | `/v1/models` | 普通本地可用模型目录；OpenAI 带 client_version 时为 Codex 动态目录；M |
| GET | `/v1/usage` | 本地用户/Key 统计；Q |
| GET | `/v1/sub2api/billing` | 当前 Key 本地计费信息；Q |
| POST | `/v1/sub2api/images/pricing`、`/v1/sub2api/videos/pricing` | 本地报价，不向供应商报价端点发请求；Q |
| GET | `/v1/sub2api/usage`、`/v1/sub2api/videos/usage` | 本地单请求图片/视频账务证据；Q |
| GET | `/v1beta/models`、`/v1beta/models/:model` | Gemini 原生目录/详情，部分条件静态后备；M |
| POST | `/v1beta/models/*modelAction` | Google 原生 generateContent、streamGenerateContent、countTokens；Gemini/Antigravity 实际分派；T |
| ANY | `/media/video-inputs/:filename` | GET/HEAD 参考图读取，其他方法 405；签名验证，不走 Key 认证和网关 Ops 中间件；R |

以下别名同样是实际注册接口，错误逻辑复用上表，并非新增一套实现：

| 前缀/别名 | 实际注册方法与路径 |
|---|---|
| 根路径文本 | POST `/responses`、`/responses/*subpath`、`/chat/completions`、`/embeddings`、`/alpha/search`；GET `/responses`（WS）、`/models` |
| 根路径图片 | POST `/images/generations`、`/images/edits`、`/images/generations/async`、`/images/edits/async`；GET `/images/tasks/:task_id` |
| 根路径视频 | POST `/videos`、`/video/generations`、`/videos/generations`、`/videos/edits`、`/videos/extensions`；GET `/videos/:request_id`、`/videos/:request_id/content` |
| `/backend-api/codex` | POST `/responses`、`/responses/*subpath`、`/alpha/search`；GET `/responses`（WS）、`/models`（Codex） |
| `/antigravity/v1` | POST `/messages`、`/messages/count_tokens`；GET `/models`、`/usage`；强制 Antigravity 平台 |
| `/antigravity/v1beta` | GET `/models`、`/models/:model`；POST `/models/*modelAction`；强制 Antigravity 平台 |
| 单独目录 | GET `/antigravity/models`；认证/分组校验，未挂网关 Ops 中间件 |

根路径没有注册 messages、usage、sub2api/billing 或 images/batches 的对应别名。依据 `gateway.go:249`、`:256`、`:302`。

功能可达性限制：异步图片依赖 Enabled/对象存储条件，批量图片受 batch_image 开关限制，WebSocket/旧版视频/搜索模拟受账号及功能配置限制。批量图片 handler/service/worker 确实由 `backend/cmd/server/wire_gen.go:276` 起注入，Gemini/Vertex 是实际适配器，不是仅存在于测试中的未接入代码。本次未确认生产启用情况。

## 3. 各类接口的错误调用链与最终协议

表中“原字段”指供应商实际提供时可存在，不能假设每家都提供。字段简写：m=message，c=code，t=type/status，p=param，d=details，rid=供应商请求 ID。

| 接口/分支 | 原始错误及中间处理 | 最终 HTTP/正文 | 字段、日志与调用方判断 |
|---|---|---|---|
| Anthropic 原生 Messages、Bedrock HTTP 错误 | HTTP 状态、头、JSON/文本正文 -> retry/failover -> 核心 handleErrorResponse | 400 原正文；其他多为重建 Anthropic `{type:error,error:{type,message}}`，状态映射见 F02 | 400 正文可保留全部原字段；其他多仅分类文案。Ops 常存原状态/m/rid，正文可选。Bedrock `gateway_bedrock.go:124` 已映射 AWS rid |
| OpenAI 原生 Responses | HTTP 错误 -> 核心/账号规则/切换；部分 OAuth/API Key passthrough 有独立 writer | 重建 OpenAI `{error:{type,message}}`；默认分支及上下文溢出例外见 F02 | c/p/d/rid 不稳定保留；上下文溢出 m 保留。不能因“passthrough”名称就认定原错误透传 |
| OpenAI 兼容 Chat/Messages | Responses/Chat 上游错误 -> `openai_gateway_upstream_errors.go:559` 兼容 mapper | 非切换路径通常保留原状态和 m，t 按状态重建；切换耗尽另行处理 | c/p/d/rid 丢失；这是部分保留，不是所有接口都固定文案 |
| Anthropic 转 Responses/Chat | 实际发 Anthropic 流，供缓冲或流式转换；异常事件未承载 | F01 可返回 200 completed/stop/DONE | 业务错误全部丢失；读错误有些仅日志后仍成功；最容易误导 SEP |
| Gemini 转 Messages/Chat | Google error.code/message/status -> 兼容映射；流内事件转换 | 标准 Google HTTP 错误通常保留 m、改协议 t/状态；未识别格式固定文案；部分流内 error 被丢弃 | Google details 等未完整保留；原生流和转换流不可混为一谈 |
| Gemini 原生 `/v1beta` | ForwardNative；retry/failover；成功流解包/转发 | 非 failover HTTP 错误 `gemini_messages_compat_service.go:1569` 原状态/正文；耗尽为 Google 通用 error | 原样分支原字段在正文；流读取错误 `:2540` 后返回，上层 `gemini_v1beta_handler.go:498` 假定已写响应，可能只剩 200 部分流；未做读中断模拟 |
| Antigravity 原生 Gemini / 兼容 Messages | 重试、账号切换、内部 response 解包、兼容错误映射 | Gemini 非切换错误可保留原正文；Messages 见 F02；流读错某些分支只发简短 `stream_read_error` | 具体原 HTTP m 可到 Ops；`antigravity_gateway_streaming.go:73` 把 canceled/deadline 都当断开，不区分所有超时来源；生产条件未确认 |
| Bedrock 流 | AWS EventStream exception/header/payload -> decoder -> SSE | `bedrock_stream.go:305` 把异常类型/payload 转成 Go error，`:123` 用 `%w` 返回；外层可能再通用化 | decoder 没有忽略 exception，不能归入“所有 Bedrock 错误被吞”。客户端仍可能只收到 handler 通用事件；AWS rid 有映射 |
| Responses/Chat/Messages 流，OpenAI 原生/兼容 | `response.failed`、error 事件、缺终止事件 -> 输出/降级 | 已写头时保持 200，传 error 或 response.failed；部分原生事件直接保留；转换后的 error 多只有 m/c或t | OpenAI 原生存在正确保留路径，见第 7 节；F01 与 F03 是独立缺口；HTTP 200 本身不是 SSE 失败丢失的证据 |
| WebSocket `/responses` | 上游 WS 错误帧、HTTP 建连错误、HTTP->WS 桥接、fallback/切换 | 升级前 HTTP JSON；升级后 101 连接内 error/response.failed 或 close；不能再返回普通 HTTP 502 | `openai_ws_forwarder_v2.go:631` 保留原错误帧；`:634` 非流缓冲转换只保留部分 m；`openai_ws_http_bridge.go:234` HTTP错误只提 m，`:391` 原事件已发送后返回错误。耗尽 close reason 在 `openai_gateway_handler.go:2433` 截断/概括 |
| count_tokens | Anthropic HTTP；OpenAI input_tokens 兼容；部分本地估算/端点不支持后备 | `gateway_count_tokens.go:227`、`:339`、`openai_gateway_count_tokens.go:133` 按状态固定 m，一般保留原状态 | c/p/d/rid 不对外完整保留；实际 m 可到日志/Ops；明确端点不支持 404 是有原因的本地兼容提示，非任意404吞掉 |
| alpha/search | 原搜索 HTTP 或 Responses web_search 转换 | 原 HTTP 错误走核心/切换；后备失败事件可 200空output | 见 F01/F03；另有 Messages 搜索模拟，不是同一端点实现 |
| Embeddings | HTTP错误非切换 -> 原样 writer；网络/读取失败 -> 固定文案；切换 -> 耗尽出口 | 非切换保留状态/正文；网络502；HTTP200业务error正文仍200 | `openai_embeddings.go:109`、`:143`、`:167`。模拟确认 HTTP200 error 被当 service 成功，但原正文尚在，区别于转换器完全删除错误 |
| OpenAI 图片生成/编辑 | API Key 原生或 OAuth Responses 图片转换；HTTP/SSE错误 -> 图片专用类型或failover | 专用非切换路径保留原状态+m/t/c/p；SSE已开始时发error；其他规则/切换可覆盖 | `openai_images_responses.go:927`、`:999`、`:1014`。d、供应商rid未输出；API Key200业务错误见F07；OAuth有明确错误识别，不能一概称无错误处理 |
| Grok 图片/视频创建/编辑/扩展/查询 | `grok_media.go:547` 错误 handler；前置 failover；成功正文原样 | 非切换 `:628` 原状态+m，t按状态生成；自定义排除状态500；切换耗尽通用 | c/p/d/rid非完整保留；成功查询正文中的任务失败仍可在原正文存在，HTTP200是查询成功 |
| 异步图片 | 本地Submit202 -> 后台执行同同步图片handler -> 存储result/error -> Get200 | 已执行失败应在任务status/error中；原执行HTTP状态在记录中，查询HTTP200 | JSON error RawMessage保留；纯文本删除；2xx业务错误误完成及后台Ops缺口见F07；任务ID不是供应商rid |
| 批量图片 | 本地提交/预留 -> Gemini/Vertex API -> worker轮询 -> 本地记录/items -> 下载 | 提交错误多固定；任务GET200携带status，items有code/message/source；文件开始后无法改状态 | mapper/公开DTO问题见F06；本地清理、队列、归属、输出不存在应按本地来源理解 |
| 原生视频创建/查询/下载 | `openai_videos.go:354` HTTP错误原样；接受任务后绑定Redis；下载io.Copy | 非2xx原状态/正文；创建无任务ID外层通用502；查询HTTP200原任务正文；成品文件代理 | 原样HTTP错误字段尚在；HTTP200无ID业务错误可只剩“无任务ID”的本地判断；绑定失败保留已接受ID并记日志；不应盲目重新提交 |
| 脑生云旧版视频创建/查询 | PNG保存/签名 -> legacyRequest -> decodeLegacyTask -> normalize -> Redis状态 | 创建接受后回执；查询200+failed/error；HTTP拒绝按F04重建 | 成功解码的task.error c/m保留，经有限脱敏；t/p/d/rid不保留；网络/错误信封原因缺失见F04 |
| 脑生云旧版成品下载 | 查任务 -> 校验VideoURL -> 文件GET -> 最多刷新一次 -> MP4流 | 未完成409；生成failed409且含task c/m；输出URL缺失503；文件源错误502/超时504；已传文件则断流 | F05；这些是生成/文件/网络不同阶段，不可都当“生成失败” |
| 模型目录 | 普通本地目录、Codex上游manifest、Google原生目录与静态后备 | 普通200可掩盖DB错误；Codex错误通常502带截断原因；Google正常原响应/指定后备200 | F11；models成功不能证明某生成路由、账号额度、模型调用可用 |
| 报价、计费信息、用量 | 本地DB/缓存/价格解析；不产生供应商报价HTTP错误 | 400/403校验，503不可用，409价格保护；正常200中可有unavailable/pending | F08/F11；unavailable=无有效报价，pending=账务未确认，不等于模型正在生成或供应商失败 |
| 参考图签名URL | 本地签名/Redis/磁盘/并发验证 | 403签名/归属、410过期、404不存在、429并发、503存储、405方法；text/plain | `video_reference.go:310`。本地错误，不存在供应商原始error；读取原因部分写日志，详见下节 |

### 3.1 公共中间件与平台前置错误

普通 `/v1` 链路由 `gateway.go:117` 起依次挂请求体限制、客户端请求ID、Ops、端点归一化、Key认证；计费查询路由在 RequireGroupAssignment 之前注册。Google链路使用独立Google认证writer。全局Recovery与RequestLogger另在server组装阶段注册。它们并非都经过同一个JSON异常函数。

| 本地错误来源 | 执行位置及最终响应 | 诊断保留与审计判断 |
|---|---|---|
| Key缺失/不存在/禁用、用户禁用、IP限制 | `backend/internal/server/middleware/api_key_auth.go:66` 起；401/403等，`middleware.go:78` 返回顶层 `{code,message}`，不是OpenAI嵌套error | 原因大多具体；未知认证依赖错误在 `api_key_auth.go:79` 统一500 Failed to validate API key，没有在此保存底层err。应标平台认证错误，不能称上游拒绝 |
| Google认证/订阅/余额 | `backend/internal/server/middleware/api_key_auth_google.go:25`，`:223` 返回 `{error:{code:<HTTP>,message,status:<Google状态>}}` | 已识别禁用、余额、到期等原因具体；未知Key校验/订阅维护依赖错误固定500。Google code是HTTP数字，不能当作供应商业务code |
| Key未分组 | `backend/internal/server/middleware/middleware.go:111`，拒绝时403、说明未分组；writer分别为Anthropic或Google格式 | 具体本地原因保留；不是供应商403；分组配置实际值未读取 |
| 请求体超过限制 | `backend/internal/server/middleware/request_body_limit.go:10` 只安装MaxBytesReader，不直接生成统一响应；具体handler读取后决定400/413/其他 | 不能仅根据该中间件宣称所有超限请求都有413。报价的4,096字节超限会被归入通用400，见F08；其他接口各有读取/上传校验 |
| 无可用账号/模型不支持 | `backend/internal/handler/no_account_error.go:58`；明确池内有账号但模型无支持时404 model_not_found+模型名；其他情况503 api_error Service temporarily unavailable | 这是本地调度诊断，不是已收到供应商HTTP503。无账号、账号暂不可调度及诊断不足可能被合并，HTTP/正文不能解释每种池内原因 |
| 并发等待队列满/并发超限 | `backend/internal/handler/concurrency_error_response.go:12`；队列满429 rate_limit_error Too many pending requests；并发超限429并说明slotType；取消499；其他异常503通用 | 队列满与活动并发不是同一个限制，已有不同message；未知基础设施/等待异常归入通用503。若SSE已提交，实际HTTP可能200，由流内writer发送错误；日志边界见F10 |
| 公共计费预检查 | `gateway_handler.go:1999`；已知额度/RPM返回429及具体message，部分带Retry-After；未知默认403 billing_error | 既有业务原因有保留；未知内部错误误归账务限制见F08。原err在某些调用点由request logger记录，如 `openai_gateway_handler.go:329`，不是所有调用点都已动态确认 |
| handler/service返回error但尚未写响应 | `openai_gateway_handler.go:2259`、`gateway_handler.go:1556` 等补写出口 | 对外通用502等，实际err依赖调用点日志；这是平台兜底，不代表供应商确实返回该状态/文案 |
| panic及文件传输中止 | `backend/internal/server/middleware/recovery.go:19`；未写响应时500管理式error信封、message=internal error；已写则Abort；ErrAbortHandler重新抛给net/http | panic诊断由Gin错误日志处理，不应将完整堆栈公开。已开始SSE不保证补发错误事件；MP4传输中止后不能附加JSON。脱敏、具体日志和关联ID仍需保证 |
| ID/端点/安全头与CORS | `request_logger.go:17`、网关client_request_id及端点中间件、`server/router.go:56`起 | 主要传递上下文或控制header，不统一转换供应商error正文；本次未验证反向代理/浏览器CORS对SEP可见响应头的额外影响 |

## 4. 响应头、字段与日志保留

### 4.1 响应头

| 实际路径 | 最终响应头策略 | 诊断信息风险 |
|---|---|---|
| 全局请求 | RequestLogger 返回 X-Request-ID；网关 ClientRequestID 另处理 X-Client-Request-ID；JSON writer 设置 application/json | 仅这些ID不能证明已携带供应商ID；模拟直接调用service未挂全局中间件，不据此断言整站无ID |
| 重建JSON错误 | 通常只有本地协议Content-Type和全局头，未统一复制原错误头 | provider request-id、限额/重试诊断头可能丢失；图片专用writer同样未写typed upstream ID |
| OpenAI passthrough错误 | JSON、no-store，并校验Retry-After；见 `openai_gateway_passthrough.go:479` | 不是完整header透传；过滤Cookie等是合理保护，抹掉所有诊断头则需另评估 |
| failover耗尽 | 部分handler通过copyFailoverRetryAfter保留最后Retry-After | 仅最后一次；不说明是哪一跳/哪一种限额 |
| 原样HTTP分支 | `responseheaders.WriteFilteredHeaders` 白名单；embeddings/native video还保留Content-Type | 正文完整不代表原始headers完整；默认保护敏感头是合理的，额外allow配置未验证 |
| SSE | text/event-stream，通常no-cache/keep-alive、禁代理缓冲；开始后HTTP200固定 | 需要流内失败终止事件，不能仅看状态码；F03直接return分支没有补事件 |
| 异步图片查询 | application/json、no-store；processing时Retry-After:3 | 返回的是本地任务状态和保存的错误，不是重新转发原HTTP响应头 |
| 旧版视频MP4下载 | video/mp4或octet-stream、no-store、nosniff、可信Content-Length等 | 文件响应不适合混入JSON；应依赖请求ID及服务端具体错误，但F05日志缺cause |
| 原生视频下载 | 选择复制Content-Type/Length/Disposition/Range、Accept-Ranges、ETag、Last-Modified、Cache-Control | 已开始后不可改状态；保留的io.Copy error有利于服务端定位 |
| 参考图片读取 | 本地文件响应或http.Error的text/plain；不走网关Ops | 存储异常并非供应商错误；`video_reference.go:348` readError会记录有限原因，但底层err不一定保留 |

### 4.2 原始字段

| 错误出口 | m | c | t | p | d | 供应商rid |
|---|---|---|---|---|---|---|
| Anthropic400 / Embeddings非切换 / NativeVideo非2xx / Gemini原生非切换原样正文 | 原正文保留 | 原正文保留 | 原正文保留 | 原正文保留 | 原正文保留 | 正文中有则保留，头受过滤 |
| OpenAI/Anthropic核心通用分支 | 常被固定文案替换 | 丢失/本地分类 | 重建 | 丢失 | 丢失 | 部分进入Ops，不统一对外 |
| OpenAI兼容Chat/Messages非切换 | 通常保留 | 丢失 | 按状态重建 | 丢失 | 丢失 | 不统一对外 |
| 图片专用非切换 | 保留 | 保留 | 保留/缺失时推断 | 保留 | 丢失 | 内部类型有，writer未输出 |
| 旧视频HTTP拒绝 | 仅顶层message；429/404例外替换 | 本地code | 本地code | 丢失 | 丢失 | 未采集 |
| 旧视频任务failed | 保留并遮蔽/截断 | 保留/缺失时明确后备 | 规范化任务status | 丢失 | 丢失 | task.id保留，但它不是provider request-id |
| 批量图片单项错误 | 可能整句替换/截断 | 解析或本地code | source/本地状态 | 未完整返回 | 未完整返回 | 不统一公开 |
| F01被忽略的流内失败 | 完全丢失 | 完全丢失 | 完全丢失 | 完全丢失 | 完全丢失 | 即使响应头有ID，也不能恢复正文原因 |

共享提取器 `gateway_upstream_response.go:288` 支持 error.message、某些嵌套JSON字符串、detail、顶层message，但不支持字符串error/纯文本；code提取器 `:310` 也不是通用错误字段解析器。提取到message不代表writer会采用message，F02/F04说明了两阶段差异。

### 4.3 日志能否补救

| 情况 | 能查到什么 | 不能保证什么 |
|---|---|---|
| 文本HTTP错误与多数failover | 原状态、账号、提取message、部分rid、可选正文、尝试事件 | 不保证全量原字段；完整正文可能未启用、被截断、被规则跳过或队列丢弃 |
| 转换器忽略流内error | 普通访问日志可能只有200/耗时；某些读错误有warning | 无法凭200访问日志恢复业务error的code/message/details |
| 旧视频嵌套error | 日志有状态、account/task/operation | reason可能为空；没有原正文与rid可恢复 |
| 旧视频task.failed | `video_legacy.go:433` 记录task/account/billingRequestID、code/reason | 只有适配器接受并保留下来的错误字段 |
| 旧视频网络/文件源拒绝/已输出后断流 | 外层概括错误，或download_interrupted的任务/账号 | 这里已丢弃的网络cause、源站正文、具体stream error |
| 批量图片provider mapper | 数据库/日志有映射后的code/message；单项失败可存细节 | 传nil cause后无法从后续记录找回原API错误 |
| 异步图片 | handler普通日志、本地task.status/error | 原202请求Ops不保证记录后台错误上下文 |
| 本地报价/用量 | 最终503/409等可由Ops捕获 | 若源handler直接丢err，Ops只能记录外部固定文案，不能恢复DB/Redis根因 |

Ops写入目标为 `ops_error_logs`，由 `ops_error_logger.go:606` 与 `ops_service.go:139` 链路管理；视频关联状态与签名参考图元数据主要走Redis，本次未读线上数据。数据库/日志记录“存在这套功能”不等于“生产该请求一定有完整记录”。

## 5. 实际原因对照表

以下字串均为模拟或代码推导例子，不是生产供应商返回。标记“模拟”已运行当前方法验证；“静态”仅根据分支确认。

| 实际原因 | 供应商原始错误/本地原始状态 | 算力平台最终响应 | 丢失或改变 | 证据 |
|---|---|---|---|---|
| 参数size不支持 | HTTP422，error含message=AUDIT_INVALID_SIZE、code=bad_size、type=invalid_request_error、param=size、details.allowed；头有rid/Retry-After | Anthropic核心：502 `{type:error,error:{type:upstream_error,message:Upstream request failed}}` | 422变502，实际m/c/t/p/d/rid与Retry-After不由该writer保留 | 模拟 |
| 同上 | 同上 | OpenAI核心：502 `{error:{type:upstream_error,message:Upstream request failed}}` | 同上 | 模拟 |
| 同上，走兼容路径 | 同上 | 422 `{error:{type:api_error,message:AUDIT_INVALID_SIZE}}` | m/HTTP保留，原t被重建，c/p/d/rid丢失 | 模拟 |
| 同上，走图片专用出口 | 同上 | 422 `{error:{type:invalid_request_error,message:AUDIT_INVALID_SIZE,code:bad_size,param:size}}` | m/c/t/p保留；d和rid未输出 | 模拟 |
| Anthropic生成中拥堵 | 200 SSE：message_start后error.type=overloaded_error，message=AUDIT_PROVIDER_OVERLOAD | Responses缓冲：200 status=completed；Responses流：response.completed；Chat：stop/DONE | 整条业务失败被删除，错误转成功 | 模拟4分支 |
| Gemini配额耗尽 | 200 SSE：error.code=429、status=RESOURCE_EXHAUSTED、message=AUDIT_PROVIDER_QUOTA | Chat流：200、空内容、stop、DONE，返回nil error | 原message/code/status完全丢失 | 模拟 |
| Alpha后备搜索失败 | Responses SSE response.failed，error.code=quota | 转换器返回 `{"output":""}`、nil error；调用方按成功处理 | 失败终止事件与原因丢失 | 模拟转换器；外层HTTP静态 |
| Embeddings业务失败使用200 | 200 `{"error":{"code":"quota","message":"AUDIT_PROVIDER_QUOTA"}}` | 200原正文；service返回成功result/nil error | 正文没丢，但成功/失败归类错误 | 模拟 |
| 旧视频参数错误，嵌套格式 | HTTP400 `{"error":{"message":"AUDIT_INVALID_SIZE","code":"bad_size"}}` | 502 error.type/code=video_upstream_error，m=`Video upstream returned HTTP 400` | 原m/c删除；日志reason也为空 | 模拟适配方法；外层信封静态 |
| 旧视频参数错误，顶层格式 | HTTP400 `{"message":"AUDIT_INVALID_SIZE","code":"bad_size"}` | 502，m=`Video upstream returned HTTP 400: AUDIT_INVALID_SIZE` | 原m保留，code删除，HTTP变502 | 模拟适配方法 |
| 旧视频额度耗尽 | HTTP429 `{"message":"AUDIT_QUOTA_EXHAUSTED"}` | 429 rate_limit_error，`Video upstream is busy` | 额度不足被概括成忙；顶层m可在该警告日志保留 | 模拟 |
| 旧视频TLS错误 | transport error AUDIT_TLS_CERTIFICATE_EXPIRED，无供应商HTTP响应 | 502 video_upstream_error，`Video upstream request failed` | cause和TLS原因删除；不能声称供应商返回502 | 模拟 |
| 旧视频业务code失败 | 预期HTTP成功状态内 `{"code":1007,"message":"AUDIT_INVALID_MODEL"}` | 502 video_protocol_error，`Invalid upstream video response` | 不把失败当成功，但业务code/message丢失 | 模拟decoder |
| 视频任务生成失败 | 已解码task.status=failed，error.code/message存在 | 查询200，status=failed，error包含处理后的code/message | 正常异步语义；不是HTTP200误判；额外fields未保留 | 静态/既有任务状态测试 |
| 视频成品链接签名过期 | 文件GET403，正文AccessDenied/ExpiredToken及请求ID | 先刷新一次，最终502 `Video file source returned HTTP 403` | HTTP性质、业务原因、源站rid删除；不应把生成任务改failed | 静态 |
| MP4传输中读取中断 | io.Copy/读取具体error | 已输出200部分MP4，连接中止 | 不能补JSON合理；日志不写具体err导致诊断丢失 | 静态 |
| 批量图片参数非法 | GeminiAPIError{StatusCode:400,Code:INVALID_ARGUMENT,Message:AUDIT_INVALID_IMAGE} | mapper变502/GEMINI_INVALID_RESPONSE/`Gemini API request failed`；公开提交再概括 | 原m/code/status及cause删除；type还可能invalid_request_error | 模拟mapper；公开出口静态 |
| 批量错误含资源路径 | `Unsupported image in projects/audit/locations/test` | `upstream provider operation failed` | 整条替换，连Unsupported image也丢失 | 模拟 |
| 本地图片分组功能没开 | 本地GroupAllowsImageGeneration=false，无供应商请求 | 403 permission_error，`Model is not available for this key` | 本地开关问题表现成模型权限概括 | 静态 |
| 本地视频报价数据读取失败 | QuoteVideoBilling返回配置/仓库错误，无供应商请求 | 409 video_pricing_changed，X-Video-Dispatch:rejected | 内部故障归为报价变化；提示重新询价不一定有用 | 静态 |
| 无有效视频报价 | amount=nil，source=fallback等本地结果 | 200 pricing_status=unavailable | 正文明确不可报价，本身不是丢错；不能解读为可提交 | 静态 |
| 账号切换全部失败 | 多次供应商错误，最后一条仍有真实正文 | 通用502/503或最后状态+`All available accounts exhausted` | 客户端失去实际失败，部分尝试可从Ops恢复 | 静态/既有耗尽测试 |

## 6. 合理脱敏、泛化、归类与丢失的区别

| 类别 | 本项目例子 | 审计判断 |
|---|---|---|
| 合理脱敏 | Ops递归遮蔽authorization/api_key/password/cookie等敏感键；旧视频精准替换本账号key | 保留诊断结构的方向合理，但文本值和非JSON仍需补齐 |
| 合理协议适配 | 原HTTP错误转目标协议error信封；异步查询200+status=failed；SSE提交后保持200、发失败终止事件 | 外层形状/HTTP语义可变，真实原因仍应保留；不能简单要求字节完全一致 |
| 合理内部信息保护 | 网络错误对外隐藏内部IP/端口、私有文件路径，服务端保留具体err | `gateway_upstream_response.go:243`、`:990` 给出断流类别，完整错误另记日志，优于直接删除整条原因 |
| 错误泛化 | 按401/403/429/5xx换固定文案；出现projects/就整句删除 | 实际诊断消失，不等同脱敏 |
| 错误归类 | 参数422变502；内部报价故障变409价格变化；批量502固定invalid_request_error；未知计费错误默认403 | 易引导客户端改错参数、重试或重新询价 |
| 完全丢失 | F01转换器忽略error、旧视频网络cause丢弃、文件源错误正文不读、下载日志不写err | 即使后续增加统一异常handler也无法恢复上一步已经删除的数据 |

敏感信息检查位置与边界：

| 敏感内容 | 可能出现位置 | 当前情况 |
|---|---|---|
| Key、Bearer、Cookie、会话token、私钥 | JSON专用字段、message/details字符串、纯文本、错误正文日志、异常字符串、header | JSON专用键和默认header白名单有保护；通用message/纯文本处理不足，不能凭HTTP状态证明安全 |
| 数据库/Redis/代理连接串 | err.Error、URL userinfo、配置/存储失败message | 部分旧视频/参考图路径直接拼接err；共享query-key正则不覆盖DSN口令 |
| 签名URL | 供应商错误引用参考图/成品URL、Ops detail、普通日志 | 旧视频整URL替换可防泄露但过度；共享sanitizer不覆盖所有sig、签名参数名称 |
| 个人信息/提示词 | 错误message、details、搜索query、请求摘录 | 没有统一文本PII处理；旧视频创建只替换匹配的完整提示词；搜索模拟 `gateway_websearch_emulation.go:158` 会写query日志 |
| 内部诊断信息 | 文件路径、私网地址、堆栈、项目/桶/资源ID | 对客户端应保留非敏感原因而隐藏内部片段；受控运维记录需要细节。不是要求将所有堆栈原样返回SEP |

## 7. 已正确保留的逻辑

以下部分无需改成新的通用错误出口；后续只补明确缺口，不应倒退：

1. **图片专用错误处理**：非切换HTTP/识别出的OAuth错误保留实际m/t/c/p及HTTP状态，已存在对应测试。缺口是details、请求ID、少数规则/耗尽覆盖，不是整套图片错误处理都不可用。
2. **原样正文分支**：Embeddings非failover、原生视频非2xx、Gemini原生非failover、Anthropic核心400能保留原错误正文。字段完整性在这些分支较好，但后续需审查敏感片段，且不能将HTTP200业务error当成功。
3. **OpenAI原生流与WS的部分错误**：`backend/internal/service/openai_gateway_response_handling.go:407` 识别/转发failed事件，避免已输出后重复追加误导性错误；`backend/internal/handler/openai_gateway_handler.go:2308` 检查错误已传递；WS原错误帧在 `openai_ws_forwarder_v2.go:631`、HTTP桥接流事件输出处有保留。不能概括成“全部流式错误只断连接”。
4. **Responses失败终止事件**：`backend/internal/handler/stream_error_event.go:55` 提供response.failed，而不是一律断开；`gateway_handler.go:1525` 和部分OpenAI调用点还用MarkOpsStreamError保留运维诊断。F03/F10是未覆盖的分支。
5. **旧视频任务failed**：`video_legacy.go:396` 保留code/message；未给message时明确说明“上游status=failed但未返回error.message”并附task ID，不伪造具体失败原因。查询200+failed、下载409+生成原因也正确区分阶段。
6. **本地具体校验**：视频重复字段、时长/分辨率、幂等冲突、身份不匹配、PNG/MP4/长度校验等很多错误已有明确本地code/message。`submission_unknown` 明确原提交待确认；不能因此让SEP盲目再次付费提交。
7. **接受任务后的关联失败**：原生视频Redis绑定失败不删除已接受task ID，避免用通用错误掩盖已经提交成功。需要运维补救，但不应将其误写成供应商拒绝。
8. **运维尝试记录**：`ops_error_logger.go:638` 即使最终成功仍可保存重试/切换的上游错误事件；`ops_upstream_context.go:115` 对流式错误保留首个根因。该机制值得保留，只需补覆盖、脱敏与截断可见性。
9. **明确本地身份/额度错误**：认证中间件已有API_KEY_REQUIRED、INVALID_API_KEY、USER_INACTIVE等；计费预检查对已识别RPM/额度错误保留具体message和Retry-After。不是所有403/429都应解释为供应商返回。

## 8. 后续建议修改范围，不在本次实施

按依赖顺序建议如下，均不要求改变现有重试次数、切换决策、计费算法或任务状态策略：

| 优先级 | 建议文件/范围 | 只针对错误完整性的目标 |
|---|---|---|
| 1 | gateway_forward_as_responses.go、gateway_forward_as_chat_completions.go、gemini_chat_completions_compat_service.go、gemini_messages_compat_service.go、openai_alpha_search.go、apicompat事件类型 | 承载并识别流内错误/失败终止；扫描异常不能静默合成completed；输出目标协议可识别的实际失败 |
| 2 | gateway_upstream_response.go、openai_gateway_upstream_errors.go、openai_gateway_passthrough.go、antigravity_gateway_streaming.go及handler耗尽出口 | 保留原message/code/type/param/details与原HTTP状态/来源；避免按状态生成不存在的具体原因；协议映射确有必要时说明映射，不改切换策略 |
| 3 | video_legacy.go、video_download.go、openai_videos.go及video handler | 兼容嵌套/字符串/顶层/纯文本错误；保留非零业务code与原因；保留网络cause；读取文件源有限错误正文；断流日志写具体err和关联ID；不改任务状态或重发策略 |
| 4 | batch_image_provider_gemini.go、batch_image_provider_vertex.go、batch_image_public.go、batch_image_handler.go、batch_image_processor.go | mapper保留cause及实际供应商原因；公开任务级错误；按来源分类；资源路径只遮蔽敏感片段；区分提交失败和释放预留失败 |
| 5 | image_task_handler.go、image_task.go | 保留执行错误与来源；检查200业务失败；确保后台诊断落库；区分生成、响应校验、结果存储失败 |
| 6 | gateway_image_billing.go、gateway_video_billing.go、gateway_key_billing.go、gateway_video_billing service、gateway_handler.go | 返回具体本地校验原因；将配置/仓库失败与价格真正变化区分；保留查询失败诊断；不改计价与结算策略 |
| 7 | 共享脱敏、ops_service.go、logger调用点、responseheaders、ops_error_logger.go | 字段与文本双层按片段脱敏；对外、内部日志、落库三处一致；保留provider rid/Retry-After等诊断；不无条件透传Cookie/堆栈/私有连接串 |
| 8 | gateway_service.go模型目录、Gemini目录后备、usage汇总 | 有意降级与依赖故障可区分；不能用默认目录/空统计暗示实际成功读取 |

后续修改应建立针对“同一个错误穿过整条链后保留了什么”的契约验证，覆盖HTTP/SSE/WS/异步/文件与重试后结果。不要仅测一个异常处理函数，也不要用新的固定文案替换旧固定文案。

## 9. 已执行验证

### 9.1 静态核对

- 核对gateway.go全部实际路由、根路径/Codex/Antigravity别名及中间件挂载；核对DI注入，区分未启用与未接入代码。
- 追踪handler -> 账号选择 -> service/供应商适配 -> retry/failover -> writer；核对普通HTTP、SSE、WS、异步回执、MP4/图片/ZIP文件响应。
- 核对Ops采集、尝试事件、错误正文脱敏、普通日志、请求ID、响应头过滤；未读取.env、真实配置凭据或生产记录。

### 9.2 既有测试

以下命令在 `D:\github\LongdaoAI\backend` 执行，Go 1.26.5；只运行unit标签与所列测试，不是全量集成测试。

```powershell
& "$env:LOCALAPPDATA\Programs\Go1.26.5\go\bin\go.exe" test -tags unit ./internal/service ./internal/handler ./internal/server/middleware ./internal/server/routes -run 'Test(ExtractUpstreamError|.*ErrorPassthrough|HandleOpenAIUpstreamTransportError|ForwardEmbeddings|LegacyVideoError|LegacyVideoFailure|LegacyTaskStates|LegacyVideoContentValidation|OpenAISessionPolicy|.*FailoverExhausted|.*Stream.*Error|Recovery|.*ErrorAlreadyCommunicated|SafeUpstreamURL)' -count=1 -timeout=180s
```

结果：service、handler、middleware通过；routes提示`[no tests to run]`，没有把它算成路由运行验证。

```powershell
& "$env:LOCALAPPDATA\Programs\Go1.26.5\go\bin\go.exe" test -tags unit ./internal/service ./internal/handler -run 'Test(OpenAIGatewayServiceForwardImages_OAuth(UpstreamHTTPErrorSurfacesRealError|NonStreamModerationBlockedReturnsClientError|NonStreamServerErrorReturnsFailoverBeforeFlush|StreamServerErrorAfterFlushDoesNotFailover)|OpenAIImagesSSEClientErrorsAreNotRetryable|AsyncImageHandlerSubmitAndPoll|HandleResponses(BufferedStreamingResponse|StreamingResponse)_PreservesMessageStartCacheUsage|HandleCCStreamingFromAnthropic_PreservesMessageStartCacheUsageAndReasoning|GatewayService_AnthropicAPIKeyPassthrough_CountTokens404PassthroughNotError)' -count=1 -timeout=120s
```

结果：service、handler通过。覆盖已有图片真实错误保留、审核拒绝、输出前/后切换边界、异步提交查询、转换器已有用量保留、明确count_tokens不支持后备等行为。

### 9.3 仓库外模拟验证

测试代码：`D:\temp\longdao-error-audit-20261118\audit_test.go`；overlay映射：同目录 `overlay.json`。Go overlay临时把该测试作为service包测试输入，**没有向项目源码添加测试文件**。

HTTP客户端均替换为内存mock，凭据均为dummy，域名使用example.invalid；没有发网络请求。模拟直接调用真实service方法，未挂全站中间件，所以其header快照仅用于判断该方法是否复制供应商头，不能代替全站响应头结论。

```powershell
& "$env:LOCALAPPDATA\Programs\Go1.26.5\go\bin\go.exe" test -overlay D:\temp\longdao-error-audit-20261118\overlay.json -tags unit ./internal/service -run '^TestAudit(Extractors|Anthropic|GeminiStream|BatchCause|Embeddings|CentralHTTP|LegacyRejection|LegacyBusiness)' -count=1 -v -timeout=90s
```

最终运行8组模拟测试全部通过，确认的是“当前存在这些问题”，并不表示已修复：

| 测试组 | 已验证结果 |
|---|---|
| ExtractorsAndSanitization | 共享字符串error/纯文本提取为空；旧视频嵌套error为空；共享文本脱敏遗漏Bearer/Cookie/DSN/邮箱 |
| AnthropicConversionsLoseInBandError | Responses/Chat的缓冲与流式4分支丢失上游error，合成成功 |
| GeminiStreamLosesBusinessError | Gemini error流经Chat转换后stop/DONE、nil error |
| BatchCauseLostAndWholeMessageRedacted | provider mapper丢cause；含projects/的诊断整句被替换 |
| EmbeddingsHTTP200ErrorIsSuccess | 保留error正文但按成功返回 |
| CentralHTTPErrorMapping | 同一422在核心、兼容、图片4分支保留情况不同，精确输出见第5节 |
| LegacyRejectionAndTransport | 顶层/嵌套/429分支差异；网络cause丢失 |
| LegacyBusinessErrorAndAlphaFallback | 旧视频业务非零code丢原因；Alpha后备失败事件变空成功结果 |

### 9.4 未验证事项与结论边界

- 未访问生产Nginx/CDN、服务、数据库、Redis和日志；不确认本地提交与生产二进制一致。
- 未确认生产账号协议、模型映射、错误透传规则、自定义状态码、监控开关、正文日志开关、保留期、额外响应头白名单，以及异步/批量/WS/搜索模拟是否启用。
- 未调用真实供应商、付费生成模型或提交异步任务；不确认供应商实际错误格式是否覆盖所有模拟样本。
- 未运行完整应用的所有分组组合端到端验证，未动态验证真实WS握手、代理缓冲、MP4大文件断流、ZIP传输、Redis/DB失效、并发竞争或生产账务变化；这些发现标为代码确认/条件风险。
- 未将所有HTTP200都判为错误：异步查询200+failed、报价200+unavailable、账务pending具有各自明确业务含义；只有被转换为正常完成或service错误分支未识别的情况才列为误判。
- 未把平台自己产生的校验、鉴权、并发、配置、存储、账务错误称为供应商原始错误。对网络故障也不假定曾收到供应商HTTP响应。
- 报告新增前工作树只有原有outputs未跟踪资料，无已修改源码；审计未提交或推送代码。
