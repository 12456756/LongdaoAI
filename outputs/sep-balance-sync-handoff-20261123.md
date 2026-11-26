# SEP 企业余额变动同步接口

日期：2026-11-23。本文为算力平台本次实现的交接契约，SEP 端接入仍需单独开发、验收。

## 账务规则

- SEP 企业的可用、冻结资金以 `sep_enterprises` 人民币账本为准，固定六位小数字符串。普通账户继续使用原美元账本。
- 算力后台对企业充值/扣减输入人民币。支付充值按原订单保存的人民币权益、汇率入账，手续费不计入权益；美元兑换码按兑换时的统一汇率入账。
- 余额、流水及同步事件同一事务提交。重试通知不再次充值、扣款或退款。
- `origin=compute` 是算力独立变动，SEP 需要导入企业公共算力资金，并记录资金来源；不擅自分配到某位成员，也不记成一笔新的银行收款。
- `origin=sep` 是 SEP 原有划转或结算的回执事件，只匹配原业务编号、确认已有本地记录，不能再次加减资金。免费流水 `delta_cny=0`，不扣付费余额。
- 对账差异继续记录，不能直接用算力余额覆盖 SEP 钱包。存量事件从本次迁移后产生，不自动导入此前的历史流水。

## 通知

管理员在“SEP 企业对接 / 余额同步”配置 `balance_webhook_url`，由算力 POST 到该地址。仅支持 HTTPS，本机测试可使用回环地址 HTTP；不跟随重定向。

请求正文为事件，不包含 API Key、密码或签名私钥。示例：

```json
{
  "contract_version": 1,
  "event_id": "ledger_example",
  "enterprise_id": "enterprise_example",
  "compute_account_id": 52,
  "sequence": "13",
  "origin": "compute",
  "type": "compute_credit",
  "currency": "CNY",
  "delta_cny": "1.000000",
  "amount_cny": "1.000000",
  "free_cny": "0.000000",
  "available_paid_cny": "11.000000",
  "frozen_paid_cny": "0.000000",
  "business_ref": "payment:123",
  "transfer_id": null,
  "execution_id": null,
  "settlement_id": null,
  "exchange_rate": "7.150000000000",
  "occurred_at": "2026-11-23T10:00:00+08:00"
}
```

`delta_cny` 带方向；`amount_cny` 为非负绝对额。`compute_credit` 为充值/退款失败恢复，`compute_debit` 为后台扣减，`compute_refund` 为支付退款扣款，`paid_consumption` 为实际付费消费。

SEP 必须在本地余额修改和事件去重记录的同一事务成功后，返回 HTTP 2xx：

```json
{"event_id":"ledger_example","status":"applied"}
```

重复事件应返回相同确认，不重复记账。仅返回 `received`、错事件编号、非 JSON 或非 2xx 均不视为入账成功。

按企业顺序发送；较早未确认事件会阻止后续事件推送。失败后退避重试，延迟上限 512 秒；服务重启会恢复数据库中的未完成事件。SEP 建议使用事务锁校验企业流水序号并推进游标，防止推送与补拉并发重复入账。

## 通知签名

算力服务端配置 `SEP_EVENT_SIGNING_PRIVATE_KEY`（Base64 的 32 字节 Ed25519 seed）和 `SEP_EVENT_SIGNING_KEY_ID`。私钥必须持久化并安全注入，不交付 SEP，不通过前端保存。公钥可从后台查看或由 SEP 带现有服务签名请求：

`GET /api/v1/sep/balance-events/signing-key`

请求头：`X-Compute-Key-ID`、`X-Compute-Timestamp`（Unix 秒）、`X-Compute-Nonce`、`X-Compute-Signature`（Base64URL，无填充）、`Idempotency-Key`（等于事件编号）。

验签原文为以下各行以 LF 连接，无末尾换行：

```text
compute-balance-event-v1
<key_id>
<timestamp>
<nonce>
POST
<URL path and query>
<SHA256(raw request body), lowercase hex>
```

SEP 校验可信公钥版本、签名、时间窗口、nonce 防重放及事件归属。金额去重必须以 `event_id` 为准，不能以 nonce 去重，因为同一事件重试会重新签名。

## 补拉与确认

以下接口继续使用既有 SEP 服务签名：

- `GET /api/v1/sep/enterprises/:enterpriseId/balance-events?after_sequence=0&limit=100`
- `POST /api/v1/sep/balance-events/:eventId/ack`，请求头 `Idempotency-Key=:eventId`，仅在本地已入账后确认。

补拉返回 `{contract_version,items:[{event_id,sequence,payload,status,attempts,last_error,next_attempt_at}],after_sequence,has_more}`，按数据库整数序号升序，每页上限 200。推送成功的事件仍可补拉，供故障恢复和核对。事件序号与企业账务流水序号相同；迁移前历史不生成事件，首次接入需双方确认起点。

## 算力平台直接消费

在管理员启用 `direct_billing_enabled` 后，企业自己的用途 Key 可显式携带 `X-Compute-Billing-Mode: direct`。生成请求必须提供唯一 `Idempotency-Key`，作为执行编号。不能同时携带 SEP 服务身份、签名或执行编号，签名请求失败不会自动降级为直接扣款。

模型、用途、价格和媒体报价校验保持现有逻辑。图片/视频先用相同用途 Key、direct 标记请求原报价接口，再将返回 quote_id 放入 `X-SEP-Quote-ID`。实际费用按快照汇率转人民币，算力直接消费不使用 SEP 免费额度。SEP 原有请求继续由 SEP 决定免费/付费分配。

实际费用大于可用余额时，最终费用保留，`settlement_status=unsettled` 和 `billing_error` 返回具体差额说明；充值后自动补扣原账单，禁止重发生成。存在欠款时阻止新的独立消费。直接计费执行不能再次提交 SEP 结算，否则返回 `DIRECT_EXECUTION_ALREADY_BILLED`。

## 后台入口与错误

- 用户管理中的 SEP 企业显示人民币余额，充值/扣减弹窗单位为人民币。
- 后台余额变动接口继续为 `POST /api/v1/admin/users/:id/balance`，SEP 请求须带唯一 `Idempotency-Key`。成功响应附 `sep_balance_receipt`，表示本地已入账和通知状态，不表示 SEP 已入账。
- 企业账务历史跳转至 SEP 企业对接页；同步表显示具体失败原因、事件编号、重试次数，可手动重试。
- 未配置接收地址：事件保持待同步，可补拉；手动推送返回 `BALANCE_WEBHOOK_NOT_CONFIGURED`。
- 充值已入账但更新支付订单完成状态失败：返回 `PAYMENT_CREDITED_STATUS_PENDING`，`accounting_status=committed`，恢复原订单不会再次入账。
- 支付退款待确认或连接结果不明：保留对应人民币扣款和原退款订单，查询原单确认后完成或恢复；不因网络超时立即返还可用余额。
- 签名私钥缺失、HTTP 拒绝、连接失败和确认正文不符分别保留实际原因，不将本地充值成功改成充值失败。
- 业务错误保留具体错误码、失败环节、业务编号、处理建议及追踪号。数据库错误包含 SQLSTATE 与脱敏后的实际原因；不返回数据库行数据、密码和 Key。提交超时标记结果未确认，使用原业务编号重试查询，不能新建操作。

## 待 SEP 配套

实现通知接收、验签、按事件唯一编号入账、企业序号校验、补拉和确认；按 origin 区分独立变动与自身操作回执，避免重复扣费。接收地址和公钥配置完成后再做双方联调，本次算力侧本地通过不代表 SEP 已接通。

## 本地验证结果

2026-11-23 使用最新构建完成以下验证：

- 后端 `service`、`repository`、`handler`、`handler/admin`、`server/middleware` 五个包完整单元回归通过；默认套件跳过 11 项依赖外部条件的测试，其中本次相关的 SEP 数据库测试已单独连接 PostgreSQL 补跑通过，真实上游测试未执行。
- 单独连接 PostgreSQL 验证账务事务回滚、幂等充值、并发扣款不透支、通知失败后的有序重试、直接消费只结算一次、欠款充值后补扣、退款扣款与恢复，以及原有免费/付费混合结算。
- N09 流水分页与注册账户身份测试通过；流水按整数序号排序。
- 支付测试确认使用原订单人民币权益与汇率快照；已入账但订单完成状态更新失败时，明确报告已入账。退款待确认或连接超时时保留原扣款、原订单，不自动发起第二次退款。
- 前端 17 项定向测试、类型检查和修改文件的 ESLint 检查通过。
- 最新本地服务实测企业人民币充值、扣减、设置余额、相同业务编号重试、不同请求冲突、余额不足的具体错误及追踪号、同步事件分页；普通账户仍按美元处理。
- 桌面和 390/360 像素宽移动页面检查通过，无页面脚本错误或页面横向溢出；原有模型基础价格在验证前后保持一致。

验证未调用真实模型生成，未产生真实生成费用。通知发送使用本地模拟接收端测试；实际预览环境尚未配置 SEP 通知地址和通知签名私钥，事件保持待同步，不能视为真实 SEP 已入账。

本地预览前端：`http://127.0.0.1:3301/admin/sep`；后端：`http://127.0.0.1:28080`。
