# OpenAI 兼容视频接口

支持 OpenAI 平台类型的 API Key 上游，包括 A6API 的 `openai-video` 模型。

| 操作 | 路径 |
| --- | --- |
| 提交生成任务 | `POST /v1/videos` |
| 提交兼容别名 | `POST /v1/video/generations` |
| 查询任务 | `GET /v1/videos/{id}` |
| 获取视频内容 | `GET /v1/videos/{id}/content` |

以上路径同时提供去掉 `/v1` 的别名。提交别名统一转发到上游 `POST /v1/videos`。
Grok 分组原有的 `/v1/videos/generations`、编辑、延长和任务查询接口继续使用 Grok 协议。

## 管理配置

1. 上游账户选择 OpenAI / API Key，Base URL 填上游根地址或 `/v1` 地址，启用“视频生成”端点能力。显式配置过旧能力列表的账户，需要手动勾选视频能力；未限制能力的账户默认支持。
2. 将账户加入 OpenAI 分组，启用该分组已有的媒体生成权限（`allow_image_generation`）。
3. 在分组的视频计费设置中填写对应分辨率的 USD/秒价格，或配置渠道模型定价。非 Grok 视频没有可靠的内置价目表：缺少定价时，提交会在调用上游前返回 `video_pricing_not_configured`。填写 0 表示明确免费；简易模式跳过定价检查。
4. 调用端必须选择视频协议。聊天客户端追加 `/chat/completions` 的行为不会因为模型名或 Base URL 改变而切换协议。

JSON、multipart/form-data 均可提交；模型映射适用于两种格式，multipart 的 `input_reference` 文件和其他供应商参数保留。建议显式指定 `seconds`（或上游支持的 `duration`），上游返回的秒数优先用于用量记录。大小、声音、参考图等能力和参数范围由具体上游模型决定。

## 账号测试入口

在账号列表打开“测试”，选择模型及“视频生成”测试模式，填写提示词、时长和尺寸后提交。仅启用视频能力的账号默认选择视频模式；同时支持聊天的账号需手动选择。

测试直接向当前上游账号提交一次 `POST /v1/videos`，会产生上游费用，不走用户余额计费。成功表示任务已受理，界面显示任务 ID 和上游状态；管理员可点击“刷新状态 / 获取视频”查询原任务，任务完成后在测试弹窗内播放或下载。查询和下载使用原上游账号，不创建新任务，也不扣中转用户余额。测试任务不绑定普通中转 API Key。请求中断后先检查原任务，避免重复付费；测试不会自动重试，后台定时测试也不会自动提交视频任务。

最近一次任务按账号保存在当前浏览器标签页的 `sessionStorage` 中，关闭弹窗或刷新页面后可恢复。升级前的任务可以填写任务 ID，点击“查询已有任务”取回结果；必须选择原提交账号。回放需要先完整下载视频到浏览器，后台服务器按流转发。开发规格与验收记录见 [管理员视频测试回放](ADMIN_VIDEO_TEST_PLAYBACK.md)。

## 调用示例

下列 `RELAY_BASE_URL` 指向自己的中转，`RELAY_API_KEY` 使用中转密钥，不使用供应商密钥。

```bash
curl "$RELAY_BASE_URL/v1/videos" \
  -H "Authorization: Bearer $RELAY_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"minimax-h3","prompt":"A paper boat floating on a calm pond.","seconds":"6","size":"1280x720"}'

curl "$RELAY_BASE_URL/v1/videos/$VIDEO_TASK_ID" \
  -H "Authorization: Bearer $RELAY_API_KEY"

curl "$RELAY_BASE_URL/v1/videos/$VIDEO_TASK_ID/content" \
  -H "Authorization: Bearer $RELAY_API_KEY" \
  -o video.mp4
```

任务响应、状态及错误正文保持上游格式；内容接口支持 `variant` 查询参数、Range 请求和流式下载。

## Seedance 参考视频

账号的视频协议选择「脑生云（旧版视频任务）」。SEP 继续调用 `POST /v1/videos`，以 JSON 提交参考素材，Longdao 将 `metadata.content` 转为脑生云「豆包 视频生成」`POST /v1/video/tasks` 的 `params.content`。

```json
{
  "model": "seedance-2.0-fast",
  "prompt": "保留参考视频的运镜，把场景改为夜间街道",
  "seconds": 5,
  "resolution": "720p",
  "aspect_ratio": "16:9",
  "metadata": {
    "content": [
      {
        "type": "video_url",
        "video_url": {"url": "https://media.example.com/reference.mp4"},
        "role": "reference_video"
      }
    ]
  }
}
```

- 可混用 `reference_image`、`reference_video`、`reference_audio`，对应的素材类型为 `image_url`、`video_url`、`audio_url`。地址支持公共 HTTP(S) 或当前上游组织可访问的 `asset://素材ID`。至少提供一张参考图或一个参考视频。提示词可省略，也可作为一条 `type: "text"` 放在数组中，与顶层 `prompt` 二选一。
- Seedance 2.0 系列支持最多 9 张图、3 个视频、3 个音频，固定时长 4–15 秒。Seedance 2.5 支持最多 30 张图、10 个视频、10 个音频，固定时长 4–30 秒。两档默认 5 秒、720p。
- 先向 `POST /v1/sub2api/videos/pricing` 提交 `model`、`operation`、`resolution`、`duration_seconds`。只要包含参考视频，`operation` 就填 `video-to-video`；提交生成时将该值和返回的报价版本分别放入 `X-Video-Operation` 和 `X-Video-Pricing-Version`。查询、下载和账务接口沿用原路径。
- `seconds: -1` 交由上游选择时长。由于上游任务响应不报告实际秒数，自动时长需配置按次渠道价，账务查询的 `duration_seconds` 为 `null`。分组视频单价支持 480p、720p、1080p；2k、4k 可通过渠道模型定价。

### 上传参考视频文件

SEP 使用同一把 Longdao API Key 调用 `POST /v1/videos/references`，以 `multipart/form-data` 的 `file` 字段上传一个 MP4 或 MOV 文件。单文件最多 90 MiB，整个请求最多 92 MiB。上传成功返回 HTTP 201，例如：

```json
{
  "reference_id": "vref_0123456789abcdef0123456789abcdef",
  "content_type": "video/mp4",
  "size_bytes": 5242880,
  "expires_at": "2026-12-01T03:00:00Z"
}
```

生成请求的 `metadata.content` 使用 `{"type":"video_url","video_url":{"reference_id":"vref_0123456789abcdef0123456789abcdef"},"role":"reference_video"}`。每条 `video_url` 的 `reference_id` 和 `url` 二选一。上传 ID 从上传日起 7 天内可用于新任务，给已派发任务签发的地址继续覆盖现有 7 天任务窗口。部署复用 `video_legacy` 的共享文件目录、容量和签名配置。

上传成功和错误都使用完整媒体回执：`operation=upload`、`submission_status=not_applicable`、`task_id=null`、`billing.status=not_charged`、`billing.actual_cost_usd="0"`。成功保留上述素材字段；失败的 `error/operation_error` 保留具体原因，敏感片段脱敏。上传与视频创建的失败缓存隔离。SEP 可以记录 `X-Client-Request-ID` 关联问题。完整字段见 `outputs/sep-media-receipt-contract-20261124.md`。

| HTTP | `error.code` | 原因 |
| --- | --- | --- |
| 400 | `invalid_video_reference_upload` | 上传格式错误、缺少文件、空文件或有多个字段 |
| 413 | `video_reference_too_large` | 文件或请求超过限制 |
| 415 | `unsupported_video_reference_format` | 视频容器无效或缺少视频轨 |
| 429 | `rate_limit_error` | 达到上传并发上限 |
| 503 | `video_reference_storage_error` | 参考视频存储暂不可用 |
| 400 | `invalid_video_reference` | 创建任务时 URL 和素材 ID 未按约定二选一 |
| 404 | `video_reference_not_found` | 素材不存在或不属于调用方 |
| 410 | `video_reference_expired` | 素材已过期，需要重新上传 |

## 任务归属与计费

任务 ID 与提交账户的绑定保存在共享 Redis 中，按中转 API Key ID 和分组隔离，保留 7 天。
查询和下载固定使用原账户，不会因为账户负载或模型调度切换到其他供应商。
另一把中转密钥或未知任务返回 404；Redis 故障返回 503。需保留 Redis 数据，清空 Redis 后不能自动找回绑定。
上游接受任务后若绑定写入失败，中转保留成功响应和任务 ID，并记录服务端错误日志；后续查询/下载需要恢复该任务绑定。

沿用现有媒体任务的提交计费方式：上游成功接受任务后记录一次用量，查询和下载不重复计费；异步失败不自动退款。
Grok 模型保留原有时长限制，其他模型的计费时长不会被截断为 15 秒。
视频用量在异步记录队列溢出时同步兜底，避免直接丢弃计费记录。

为避免重复付费，提交发生网络错误或上游错误时不自动切换账户重试；客户端提供的 `Idempotency-Key` 会透传给上游，是否支持幂等由供应商决定。

## 验证

本地回归覆盖 JSON/multipart、模型映射、路由、任务账户绑定、跨密钥隔离、上游错误、内容下载、计费和前端能力设置。

真实测试为显式启用，默认测试不会调用付费上游：

```bash
# 将供应商密钥放入环境变量 A6API_VIDEO_TEST_KEY。
# 此测试创建一次 6 秒 720p 的 minimax-h3 视频任务，会产生上游费用。
go test ./internal/service -run '^TestOpenAIVideosA6Live$' -count=1 -v -timeout=22m
```

`A6API_VIDEO_TEST_OUTPUT` 可指定响应和 MP4 的保存目录；中断后可设置 `A6API_VIDEO_TASK_ID` 继续查询同一任务，避免再次提交。
