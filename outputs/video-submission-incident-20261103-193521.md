# 真实视频请求失败排查

事件时间：2026-11-03 19:35:21，Asia/Shanghai。排查约 19:50—20:01，生产只读；未重放生成 POST、未释放预留、未修改生产配置或应用代码。

## 结论与证据边界

已经证实新服务器出口至 `api.a6api.com:443` 的 TLS/SNI 路径异常：实际 sub2api 容器中 TCP 可以建连，但携带该域名的 TLS ClientHello 发出后即收到连接重置，TLS 未完成，没有 HTTP 响应。旧服务器对同一域名、同一两个 IPv4 节点均能完成证书校验并得到 HTTP 401；使用账号 35 的原凭证只读查询模型目录得到 200，包含 minimax-h3。故不能把当前故障归为模型不支持、账号密钥无效或请求 JSON 参数被供应商拒绝。

原始 19:35 请求确认是 upstream transport error，未收到可解析 HTTP 响应。其日志没有 httptrace/TLS 阶段信息或原始抓包，不能严格还原原 POST 是否已经写入。当前只读探测以近似延迟稳定复现握手重置，因此原单同样在握手中断是高置信推断，不能据此直接认定供应商未接单或自动释放资金。

RST 注入/路径过滤是强假设：探测中真实 SYN-ACK TTL=46，ClientHello 后约 4ms 出现 TTL=94/142 的多个 RST，随后约 96ms 又收到 TTL=46 的真实对端 ACK。这比单纯供应商应用拒绝更符合链路中间设备干预，但具体是哪个网络设备、运营方或供应商边缘策略，当前未取得对端证据，不能最终归属。

## 请求关联

| 项目 | 值 |
|---|---|
| 中转 request_id | 6ee7be85-53dc-42aa-a157-09c03d6b10c0 |
| 中转 client_request_id / 发出幂等键 | 5874aeb4-d359-4da3-8d7c-8ba867dee104 |
| 账号 / Key / 分组 | 35 / 68 / 39 |
| SEP 回执 | cmw0m2oe6013wpk018xbm6j6c |
| SEP 内部 clientRequestId | video_request_42247014-6bd4-4daa-8872-9dfd9ce0c0b4 |
| SEP 保存的 upstreamRequestId | client:5874aeb4-d359-4da3-8d7c-8ba867dee104 |

`client:...` 是 sub2api 客户关联/计费查找 ID，不是供应商 task ID。SEP 向中转发送 X-Client-Request-ID 和 Idempotency-Key；中转继续向供应商转发 Idempotency-Key，但该视频路径不转发 X-Client-Request-ID。供应商应优先检索无 client: 前缀的幂等键，不能假定供应商实现了幂等去重。

原链路：SEP → 当前连接 `https://ai.longdaoec.com/v1` → 新机入口 → `longdao-ai-blue` → account 35 → `https://api.a6api.com/v1/videos`。该容器为独立 Docker 的 host network；account 35 proxy_id=NULL，无账号代理。实测出口及华为云元数据 public-ipv4 均为 121.37.128.126；日志私网源地址为 192.168.0.139。

## 原始生产日志与错误记录

- 19:35:21.342：SEP 回执创建；19:35:21.371 预留资金。
- 19:35:21.399：中转内容审核开始并允许，body_bytes=701。该数值是审核载荷大小，不能当作原始 HTTP JSON 字节数。
- 19:35:21.514：`handler/openai_videos.go:164`，`openai_videos.forward_failed`：

```text
video upstream request failed:
Post "https://api.a6api.com/v1/videos":
read tcp 192.168.0.139:53232->107.190.225.158:443:
read: connection reset by peer
```

- 19:35:21.516：中转返回 502，latency_ms=117。
- ops_error_logs id=24358：upstream_error / upstream phase，upstream_latency_ms=107；upstream_status_code=NULL，network_error_type 未记录，错误详情被压缩成 Video upstream request failed。

`ForwardVideo` 的 `httpUpstream.Do` 返回 error 后立即结束；不会进入 HTTP status/body 处理，更没有上游 task ID 返回记录。117ms 是中转整次处理延迟，不能当作供应商生成耗时。

## 容器网络实测

全为不带凭证的 GET/握手；模型目录鉴权读取只在已可达的旧服务器上进行。

| 位置 / 测试 | 结果 |
|---|---|
| 新容器默认 DNS，GET /v1/models | TLS 未完成，HTTP 000，约 150ms，curl 35 / reset |
| 新容器固定 107.190.225.158，SNI api.a6api.com | TCP 约108ms，TLS=0，HTTP 000，reset |
| 新容器固定 162.4.194.47，同 SNI | TCP 约103ms，TLS=0，HTTP 000，reset |
| 新容器固定旧机解析的 182.16.62.234，同 SNI | TCP 约165ms，TLS=0，HTTP 000，reset |
| 新机同两个节点，不带 SNI | 能取得 TLS 服务端响应和默认证书；并非对 api.a6api.com 的有效证书校验，不作业务成功证据 |
| 新机同两个节点，SNI aswe.a6api.top | TLS alert internal error；该 CNAME 不能直接作为可用替代入口 |
| 新容器 HTTPS 国内控制站 | HTTP 200，证书验证成功 |
| 旧服务器固定 107.190.225.158 / 162.4.194.47 | 两者证书验证通过，HTTP 401，TLS 约68/75ms |
| 旧服务器默认解析，GET /v1/models | HTTP 401，远端 182.16.62.234 |
| 旧服务器以 account 35 原凭证 GET /v1/models | HTTP 200，4 个模型，包含 minimax-h3 |

新容器 resolver=127.0.0.53。域名 CNAME 为 aswe.a6api.top；新机观测 IPv4 为 107.190.225.158 / 162.4.194.47，IPv6 出现 2a03:2880:f126:83:face:b00c:0:25de。旧机系统解析及旧机 Google DoH 返回 172.80.127.125 / 172.80.127.126 / 182.16.62.234，DoH AAAA 没有地址记录。IPv6 解析存在异常或不一致，但原单实际使用 IPv4；两个原 IPv4 在旧服务器均能正常服务，所以不能仅凭不同 A 记录宣布 DNS 污染是唯一根因。新机固定旧机 IPv4 后仍失败，单独固定 IP/改 DNS 尚不足以修复。

19:57:35 的只读探测抓包摘要（非原始19:35付费请求）：

```text
19:57:35.083201 local:47908 -> 107.190.225.158:443 SYN
19:57:35.177070 peer -> local SYN-ACK ttl=46
19:57:35.180437 local -> peer 517-byte TLS ClientHello
19:57:35.184372 peer-address -> local RST ttl=94
19:57:35.185966 peer-address -> local RST-ACK ttl=142 (重复3次)
19:57:35.276056 peer -> local ACK ttl=46, ack=518
```

没有 ServerHello 或业务 HTTP 发送。此探测捕获 10 包、无丢包。未保存原始付费请求抓包。

## 同账号影响范围

最近成功的中转视频 usage 记录：2026-10-30 16:47:53，account=35、api_key=68、minimax-h3，4s/720p，客户中转计费 USD4；10-29 同账号还有8笔。数据库中 11-03 没有该账号成功的视频 usage 记录。成功 usage 证明中转成功接收提交响应并记账，不等同于视频最终交付成功。

新机相关失败：

| 时间 | 请求 / 目标 | 结果 |
|---|---|---|
| 18:14:20 | 5a539fad... / 174.37.154.236 | TCP connect timeout |
| 19:13:23 | 94da92c8... / 107.190.225.158 | TCP read reset |
| 19:18:26 | 2953f2ae... / 108.160.163.117 | TCP connect timeout |
| 19:35:21 | 本单 / 107.190.225.158 | TCP read reset |

15:20/15:51另有两笔400记录，不与四笔传输故障混为同一原因。当前证据说明不是本单孤立故障，覆盖该供应商新出口链路；不能扩展成所有供应商都不可达。

## 请求路径、字段与供应商协议

account 35 当前 active、OpenAI apikey、openai_capabilities=[videos]；base_url=https://api.a6api.com/；minimax-h3 映射到自身。构造后的 `/v1/videos` 无重复 `/v1`，与 2026-10-20 的既有成功生成验证路径相同。

生产 SEP 适配器源码与本地核查源码 SHA256 一致；中转相关四个视频转发/计费文件与运行镜像版本标签 v2026.10.21-2 无差异。

本单冻结配置是 protocolVersion=3、specRevision=3。由冻结配置及实际生产适配器重建的字段如下；这是代码路径重建，不是已保存的原始 HTTP 报文，prompt 未持久化：

```json
{
  "model": "minimax-h3",
  "prompt": "<未持久化，不记录用户提示词>",
  "seconds": 4,
  "resolution": "720p",
  "aspect_ratio": "16:9",
  "fps": 24,
  "generate_audio": true,
  "output_format": "video/mp4"
}
```

Content-Type=application/json；Accept=application/json；Authorization=Bearer <脱敏>；Idempotency-Key=5874aeb4-d359-4da3-8d7c-8ba867dee104。因为没有 width/height，V3 不会额外生成 size。中转不做 chat/responses 转换或音频/时长二次转换，模型映射一致则 body 原样转发。

参数没有被供应商 HTTP 400/422 明确拒绝的证据。旧验证曾以 seconds=6 / size=1280x720 成功提交，但产物实际15秒/1344x768，因此不能凭旧验证就保证本单 V3 的 fps/aspect_ratio/output_format 会被供应商兑现；网络恢复后需核对供应商正式字段协议，不能把报价成功当作全字段验证成功。

## 供应商接单与费用：当前能确认到哪里

以账号35原凭证从旧服务器只读查询供应商：GET /v1/models=200，GET /v1/videos?limit=20=404（api_route_not_found，Invalid URL GET /v1/videos）。供应商没有通过该接口提供可列举的历史任务；现有权限/接口未拿到供应商控制台账单或按幂等键反查的记录。

因此原单供应商是否接单、是否产生供应商费用，**仍待供应商确认**。缺少 task ID、没有中转 usage、当前复现握手失败都不能单独充当原单未接单证明。查询条件应是：2026-11-03 19:35:21 Asia/Shanghai（11:35:21 UTC），出口121.37.128.126，账号35对应的供应商账户，POST /v1/videos、模型minimax-h3、Idempotency-Key=5874aeb4-d359-4da3-8d7c-8ba867dee104、4秒/720p。先按时间±2分钟检索供应商边缘访问、TLS及任务/扣费日志；如有 task ID，再恢复原任务查询，禁止重新生成。

## usage 200 却永远 pending 的具体原因

这笔关联ID在 usage_logs、usage_billing_dedup、usage_billing_dedup_archive 中均为0行，**中转没有确认扣款**。SEP冻结的 USD4/¥28.80 是报价及预留，不能当作已产生的供应商费用。

实际以 api_key68 GET `/v1/sub2api/videos/usage?request_id=client%3A5874aeb4-d359-4da3-8d7c-8ba867dee104`：

```json
{
  "object": "sub2api.video_request_usage",
  "schema_version": 1,
  "currency": "USD",
  "request_id": "client:5874aeb4-d359-4da3-8d7c-8ba867dee104",
  "status": "pending",
  "model": null,
  "actual_cost_usd": null,
  "video_count": null,
  "duration_seconds": null,
  "resolution": null
}
```

接口只查中转本地 usage+交易去重，不访问供应商账单。只有 ForwardVideo 成功取得 task ID 后才写 Redis 任务绑定，并由 handler 提交成功用量记账。transport error 路径只有日志/ops_error，不落可对账的 dispatch attempt。usage 对任何“没有可靠账单”统一200/pending，未提供 not_submitted / rejected / unknown / accepted / failed 等接单终态，故此错误链路没有自然收敛机制。

SEP 回执实际存在 mediaGatewayRequest，而不是 mediaRealRequest。20:00:39 只读复查仍为 SUBMISSION_UNKNOWN / RECONCILING、dispatchState=DISPATCHING、upstreamTaskId=null；配置最后更新时间19:46:59。预留状态 RESERVED、reservedCny=28.8，settledCny/settledUsd=NULL。reconcile 仅收到 settled 且模型/时长/分辨率/费用匹配才结算，releaseUnsubmitted 仅允许 NOT_DISPATCHED/REJECTED，当前 DISPATCHING 不自动释放是现有保护行为。

## 修复与本单收尾

1. **连通性**：为 account35 使用经验证可访问的出站路径，或向供应商取得正式、国内可达且证书有效的入口。先从实际容器验证带正确SNI的TLS、鉴权模型目录及报价，再安排经授权的新任务验证。当前只改DNS/固定IP已不能解决全部问题；不要直接把CNAME当可用业务入口。可先限制该供应商的新建任务以避免更多挂起，需另行实施，本次未改变调度。
2. **可观测性**：为视频 RoundTrip 增加 httptrace 的 DNS/TCP/TLS/连接复用/WroteRequest/GotFirstResponseByte 记录，保存脱敏上游请求ID、幂等键、endpoint、账号及错误阶段到ops记录。原单缺失的阶段证据无法靠事后日志补回。
3. **可恢复对账**：发出前持久化视频 dispatch attempt，明确未发送、发送不确定、已接单、明确拒绝及账务状态；收到task ID后原子关联。usage返回持久化状态与证据来源，保留旧客户端兼容；SEP UsageSchema 当前只接受 pending/settled，协议扩展需要中转和SEP一起改，不能单边增加状态后上线。unknown增加限频后台核对、告警和人工结案入口，不能只无限轮询成功账单表。
4. **本单暂不重放、不扣结算、不释放**：供应商确认未接单且未计费后，以带证据的正式人工/补偿流程标记未接单并释放原¥28.80，不能只把DB dispatchState改成REJECTED触发退款。若确认已接单，补回task ID及供应商关联信息，查询原任务并核对中转计费账，再按现有结算流程处理；若已收费但任务失败，按供应商退款和平台计费政策另行处理。若供应商始终无法确认，转为明确的人工待核对工单，由运营决策并留审计，不能把无记录当作0费用。

## 代码证据

- sub2api `backend/internal/service/openai_videos.go:183`：路径构造；`:210`：ForwardVideo；`:255`：请求头/幂等键；`:291`：代理与Do；`:298`：transport error不重试；`:317`：task ID与绑定。
- sub2api `backend/internal/handler/openai_videos.go:162`：失败502；`:180`：仅成功请求RecordUsage。
- sub2api `backend/internal/repository/http_upstream.go:181`：HTTP客户端调用，error返回nil response。
- sub2api `backend/internal/handler/gateway_video_billing.go:156`：usage默认pending，满足可靠账单才settled。
- sub2api `backend/internal/repository/gateway_video_billing.go:15`：仅查询usage_logs+dedup证据。
- SEP `backend/src/modules/media-gateway/sub2api-video.adapter.ts:130`：提交头及JSON；`:246`：V3字段映射；`:276`：502转换VideoDispatchError；`:32`：UsageSchema只接受pending/settled。
- SEP `backend/src/modules/media-gateway/media-video-real.service.ts:205`：提交前DISPATCHING；`:228`：模糊提交保留未知；`:353`：对账；`:389`：仅明确未提交可释放。

应用代码未修改，因此未运行构建/单元测试；验证来自真实只读数据库、容器日志、两个出口网络对照、TLS握手/探测抓包、鉴权只读API及源码版本比对。供应商原单接单/费用仍未获得权威记录。
