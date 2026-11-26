-- Repair stored direct-billing receipts without replaying any accounting operation.
UPDATE sep_settlements AS s
SET receipt = jsonb_set(
    s.receipt,
    '{committed_at}',
    to_jsonb(to_char(s.created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'))
)
FROM sep_executions AS x
WHERE x.id = s.execution_pk
  AND x.billing_origin = 'compute'
  AND s.receipt->>'status' = 'committed'
  AND COALESCE(s.receipt->>'committed_at', '') = '';
