ALTER TABLE sep_executions ADD COLUMN group_id BIGINT REFERENCES groups(id);
CREATE INDEX sep_executions_pending_idx ON sep_executions(integration_id,execution_id) WHERE fee_status<>'final';
