# 脑生云豆包视频接入核查

核查日期：2026-11-11。范围：供应商公开文档、本项目源码、无凭据的空请求探测。未提交真实视频生成任务，未验证当前上游账号权限或修改生产配置。

## 结论

脑生云文档提供豆包 Seedance 视频生成。本项目具备与其新版协议相同的创建和查询路径，但存在参数、计费识别、视频下载的兼容缺口。目前不能认定已能直接使用，也不能仅凭无凭据探测的 404 认定该服务对已授权账号不可用。

## 供应商文档

控制台文档入口实际嵌入以下文档站：

- 创建：https://doc.naoshengyun.com/514596517e0.md
- 查询：https://doc.naoshengyun.com/514597368e0.md
- 旧版创建：https://doc.naoshengyun.com/499628413e0.md
- 文档目录：https://doc.naoshengyun.com/llms.txt

新版 Base URL 为 `https://api.naoshengyun.com`，创建 `POST /v1/videos`，查询 `GET /v1/videos/{task_id}`。文档模型包括：

- `doubao-seedance-2-0-260128`
- `doubao-seedance-2-0-fast-260128`
- `doubao-seedance-2-0-mini-260615`
- `doubao-seedance-2-5-260628`

文档要求请求模型由已启用的 Doubao task plugin 声明；文档列出模型不等于当前账号已经开通。

文档基础请求示例：

```json
{
  "model": "doubao-seedance-2-0-260128",
  "prompt": "一只橘猫在阳光下的窗台上伸懒腰，电影感",
  "seconds": 5,
  "metadata": {
    "resolution": "720p",
    "ratio": "16:9",
    "generate_audio": true
  }
}
```

`seconds` 为整数；扩展参数位于 `metadata`。查询成功的视频地址为 `metadata.url`。这两份新版文档没有承诺 `/v1/videos/{id}/content` 下载接口。

旧版 `POST /v1/video/tasks` 使用短模型名（如 `seedance-2.0`）和 `model_id`、`params` 请求结构；不能将旧版请求体直接发送到新版接口。本项目当前没有这一旧版协议的转换实现。

## 当前公开入口探测

北京时间 2026-11-11 14:52:46 左右从当前开发机发起：

```text
POST https://api.naoshengyun.com/v1/videos
Content-Type: application/json
Accept: application/json
无 Authorization
Body: {}

HTTP 404
Content-Type: text/plain
X-Request-ID: 8a56ae2e-01d0-40d0-b78b-33062b4018f3

404 page not found
```

这次探测没有合法模型、提示词或凭据，不构成真实生成验证。它只能说明当前开发机的无凭据请求得到上述响应；不能排除按身份、来源或账号启用不同路由的情况。需要供应商确认新版入口、账号的视频插件权限和可用模型。

仓库 `outputs/video-account48-404-20261103.md` 另记载了 11 月 3 日带账号凭据的空请求同样返回 404；那是历史证据，不能替代当前账号验证。

## 本项目兼容性

| 项目 | 当前实现与影响 |
| --- | --- |
| 创建和查询 | 支持 `/v1/videos` 和 `/v1/videos/{id}`，创建可读取上游顶层 `id`。 |
| 模型名称 | 没有图片那种自动去尺寸后缀逻辑；账号或渠道的显式模型映射仍可改名。 |
| 基础文本生成 | 上面的固定 5 秒、720p JSON 可通过当前解析规则，`metadata` 随原请求体转发；是否受理仍取决于上游。 |
| 管理后台测试 | `account_test_service_openai_video.go:244` 构造字符串 `seconds` 和顶层 `size`，没有构造新版要求的 `metadata`，存在兼容差异。 |
| 分辨率计费 | `openai_videos.go:55` 仅解析顶层 `resolution/size`，不读 `metadata.resolution`；缺省按 720p。只在 metadata 指定 1080p 时可能产生生成规格与平台计费规格不一致。 |
| 智能时长 | `openai_videos.go:109` 拒绝 `seconds=-1`，与上游智能时长能力不兼容。 |
| 无提示词请求 | `openai_videos.go:104` 强制 prompt 非空，阻止上游允许的部分参考素材请求及 Seedance 2.5 Final 请求。 |
| 回放和下载 | 平台及后台测试直接请求上游 `/content`，没有根据 `metadata.url` 获取视频；即使任务成功，后台回放也可能失败。 |

平台账号还需选择 OpenAI / API Key、具备 videos 能力、加入正确分组；分组需开启媒体权限（当前字段为 `allow_image_generation`）并配置视频定价。详见 `backend/OPENAI_VIDEOS.md`。

建议先由供应商确认当前账号可用的新接口及模型名，再适配管理测试参数、metadata 分辨率计费和下载方式，最后执行一次创建、轮询、取视频的完整验证。
