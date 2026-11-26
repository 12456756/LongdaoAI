# SEP 共享资金与欠费恢复交付

日期：2026-11-25。

本次算力后端增加持久待结算原决定。SEP 结算请求余额不足时，在事务内保留原结算编号、执行编号、请求哈希和付费金额，返回具体不足原因。本次不部分扣款、不重试模型生成。余额补足后后台通过原结算请求结清，重复恢复只记一次流水。

例：原费用 30 元、当前余额 20 元，则待结算 30 元，资金缺口 10 元，尚未扣款；补足后整单扣 30 元。不得把“缺口 10 元”显示为只需要结算 10 元。

有待结清费用时暂停新增模型生成，保留查账、目录、报价、原单恢复；原有免费费用单仍记录免费，不扣企业余额。新错误 `ENTERPRISE_BILL_OUTSTANDING` 包含恢复动作。SEP 企业直接消费仍使用现有独立扣款通知；SEP 将依规则扣公共资金并平均分摊成员企业资金，算力平台不处理成员分配。

新增迁移：`backend/migrations/195_sep_pending_settlements.sql`。修改涉及 `sep_pending_settlements.go`、`sep_ledger.go`、`sep_execution.go`、`sep_direct.go`、`sep_balance_events.go` 和 `sep_types.go`。现有余额通知与服务签名协议不变。

`TestSEP` 相关 service、handler、middleware 测试通过；新增 PostgreSQL 测试覆盖整单不足、不可变编号、先后结算、免费单、并发不足、充值后只补原单、审计失败回滚、欠费时拒绝生成但允许查询。Go 服务编译通过。

不预占资金时，已同时发出的多个请求仍可能产生高于现有余额的真实费用；本次通过保存欠费、暂停后续生成、补款恢复处理，不承诺跨库瞬时原子性。没有调用真实供应商。本次代码未提交、未合并或推送。

完整业务规则、SEP 字段与操作说明：`D:/github/SEP-worktree-two/docs/development/sep-shared-compute-funds-and-debt-recovery-2026-11-25.md`。
