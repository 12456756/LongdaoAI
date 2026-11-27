-- Keep submission recovery independent of current pricing and credential groups.
ALTER TABLE sep_executions ADD COLUMN IF NOT EXISTS client_request_id VARCHAR(256);
ALTER TABLE sep_executions ADD COLUMN IF NOT EXISTS idempotency_key VARCHAR(256);
ALTER TABLE sep_executions ADD COLUMN IF NOT EXISTS request_path TEXT;
CREATE INDEX IF NOT EXISTS sep_executions_media_request_idx
    ON sep_executions(enterprise_pk,api_key_id,kind,client_request_id)
    WHERE client_request_id IS NOT NULL;
