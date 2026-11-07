# SEP 视频工具不可用排查记录

排查日期：2026-11-03；以下事件时间均为北京时间。

## 结论

订阅 `cmv4vqty6008fo601x5y5h7aa` 的视频 Profile `auto-video-f5fd6c27a5bb8cbb0b7b11c6`（`minimax-h3`）已启用且企业已授权，但 `verified=false`。直接拦截码是 `PROFILE_UNVERIFIED`。

18:22:34 更新视频连接地址时，服务端按设计撤销了所有视频 Profile 的验证状态。后续发现模型没有重新验证 Profile，因此 18:34 客户端拿不到可执行视频工具。当前证据支持配置验证状态失效，不能据此判断上游视频执行是否正常。

初次诊断仅查询生产数据库、日志并调用只读 runtime 服务方法；当时未修改生产配置或执行 Profile 校验。用户随后要求重新校验，恢复结果见文末。两个阶段均未提交付费视频生成。

## 生产证据

- 18:22:34.243：数据库视频连接更新，当前地址 `https://ai.longdaoec.com/v1`，数据库凭证已配置且能够由 SettingService 解密；不记录凭证值。
- 18:22:34.246：目标 Profile 更新，`enabled=true`、`verified=false`，`configVersion=cfg_8112970d-9482-49a5-8cf6-4eae768bb973`。
- 18:22:34.247：审计事件 `MEDIA_CONNECTION_UPDATED`、`resourceId=MEDIA_VIDEO_BASE_URL`、`result=SUCCESS`；metadata：`kind=video`、`addressChanged=true`、`credentialChanged=false`、`previousSource=database`、`source=database`。
- 18:22:36：`POST /api/admin/media/video/models/discover` 返回 201。18:22—18:35 的相关 HTTP 日志没有出现 Profile verify 请求。
- 18:34:11、18:34:39、18:34:42：目标订阅 runtime V3 请求返回 200。
- 18:59:30：在生产容器中仅实例化配置服务，执行实际源码的 `getRuntimeVideoV3`，获得以下字段。它是当前状态的同路径服务只读重算，并非保存下来的 18:34 HTTP 响应原文；现有 HTTP 日志未记录响应体。

## runtime.video 关键字段原样摘录

```json
{
  "protocolVersion": 3,
  "executionMode": "real",
  "executionAvailable": false,
  "defaultProfileId": "auto-video-f5fd6c27a5bb8cbb0b7b11c6",
  "tools": [],
  "profiles": [
    {
      "profileId": "auto-video-f5fd6c27a5bb8cbb0b7b11c6",
      "configVersion": "cfg_8112970d-9482-49a5-8cf6-4eae768bb973",
      "kind": "video",
      "displayName": "minimax-h3",
      "operations": ["image-to-video", "text-to-video"],
      "executionMode": "async-query",
      "authorized": true,
      "adapter": {
        "id": "sub2api-video",
        "version": "3",
        "available": false,
        "reason": "PROFILE_UNVERIFIED",
        "operations": ["image-to-video", "text-to-video"],
        "referenceSupport": {
          "maxCount": 1,
          "maxInputBytes": 10485760,
          "mimeTypes": ["image/png"]
        },
        "supportsTaskQuery": true,
        "supportsCancel": false
      },
      "unavailableReason": "PROFILE_UNVERIFIED"
    }
  ]
}
```

摘录省略了 `capabilityHints`；原始服务返回的提示为：4 秒、16:9、720p、24fps、带音频、video/mp4。

企业策略已包含该 Profile，并将其设为默认；允许图生视频及文生视频，企业单次时长上限为 12 秒。订阅状态 ACTIVE。

## 判定链

1. `SettingService.updateMediaConnection` 在地址或有效凭证变化时，事务内将相同 kind 的 Profile 设置为 `verified=false` 并更新 configVersion，同时写入连接变更审计。
2. `MediaVideoConfigService.getRuntimeVideoV3` 首先检查启用，再检查验证状态。当前命中 `PROFILE_UNVERIFIED`，所以 adapter.available=false，最终 executionAvailable=false、tools=[]。
3. `executionMode=real` 仅表示真实执行总开关已打开且连接配置可解析；该检查不执行上游健康探测，也不保证 Profile 已验证。
4. 服务端仍保留两项 operations。客户端 `sep-client/src/shared/video-media.ts` 按 adapter.available 等字段过滤后，将其转换为空列表，符合当前执行门禁。
5. “发现模型”维护候选和兼容状态，不会将未验证 Profile 自动恢复成已验证。

## 所需配置调整

在平台的视频模型配置中，对现有 Profile 执行正式“校验”，保持其启用及企业授权，然后让客户端重新获取 runtime。

对应管理接口：`POST /api/admin/media/video/profiles/auto-video-f5fd6c27a5bb8cbb0b7b11c6/verify`。按当前实现，校验会检查文件缓存、模型目录及图生/文生两种操作的报价；成功后才写入 verified=true，并返回 executionTested=false。它不提交真实视频生成任务。本次没有调用该写入接口。

若校验失败，应按校验接口的具体错误继续处理连接、授权、报价或缓存问题，不能直接改库绕过 verified 门禁。若校验成功且无并发配置变化，重新获取 runtime 应得到 adapter.available=true、executionAvailable=true，以及 video_from_image、video_generate、video_get_job。

“一分钟视频”另有规格问题：当前 Profile 声明 4 秒片段，企业策略上限为 12 秒。恢复工具后仍需确认是否采用分段生成与合成；本次没有验证上游是否接受直接生成 60 秒。

## 代码定位

- `/Users/yao/LLM/SEP/backend/src/modules/setting/setting.service.ts`：连接变更撤销验证。
- `/Users/yao/LLM/SEP/backend/src/modules/media-config/media-video-config.service.ts`：verifyProfile、discoverModels、getRuntimeVideoV3。
- `/Users/yao/LLM/SEP/backend/src/modules/media-config/media-video-config.controller.ts`：校验管理接口。
- `/Users/yao/LLM/sep-client/src/shared/video-media.ts`：V3 转换时按 adapter 门禁过滤 operations。

未修改应用代码，未运行构建或单元测试；验证依据是生产 Prisma 只读查询、审计/HTTP 日志、实际配置服务只读重算及服务端/客户端代码核对。

## 19:05:44 重新校验及恢复结果

用户要求重新执行校验后，在生产容器调用正式应用服务 `verifyProfile`，携带校验前 configVersion，并完成该方法内的目录、报价及缓存检查；未直接改库绕过验证。

- checkedAt：2026-11-03T11:05:44.711Z，即北京时间 19:05:44。
- 结果：enabled=true、verified=true、executionTested=false、mock=false。
- 新 configVersion：`cfg_2f197791-41cd-4176-8c7c-8737b138a5c1`。
- 对同一订阅重算 runtime：executionMode=real、executionAvailable=true。
- tools：video_from_image、video_generate、video_get_job。
- 目标 adapter.available=true，reason/unavailableReason 已消失，图生与文生 operations 均保留。

无需修改应用代码或重启服务。客户端需重新获取 runtime 后重试；当前尚未在员工客户端发起端到端生成。完整脱敏结果保存于同目录 `sep-video-verification-20261103.json`。
