CREATE TABLE sep_pending_settlements (
    integration_id VARCHAR(100) NOT NULL,
    settlement_id VARCHAR(128) NOT NULL,
    enterprise_pk BIGINT NOT NULL REFERENCES sep_enterprises(id),
    execution_pk VARCHAR(64) NOT NULL REFERENCES sep_executions(id),
    execution_id VARCHAR(128) NOT NULL,
    request_hash CHAR(64) NOT NULL,
    request JSONB NOT NULL,
    paid_cny NUMERIC(24,6) NOT NULL CHECK (paid_cny > 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (integration_id, settlement_id),
    UNIQUE (execution_pk)
);
CREATE INDEX sep_pending_settlements_enterprise_idx ON sep_pending_settlements(enterprise_pk, created_at, settlement_id);
