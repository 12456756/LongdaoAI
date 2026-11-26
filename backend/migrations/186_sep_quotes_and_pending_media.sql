ALTER TABLE sep_executions ADD COLUMN pending_usage JSONB;
CREATE TABLE sep_quotes (
    id VARCHAR(80) PRIMARY KEY,
    integration_id VARCHAR(100) NOT NULL,
    enterprise_pk BIGINT NOT NULL REFERENCES sep_enterprises(id),
    api_key_id BIGINT NOT NULL REFERENCES api_keys(id),
    kind VARCHAR(10) NOT NULL,
    specification JSONB NOT NULL,
    native_version TEXT NOT NULL,
    exchange_rate NUMERIC(24,12) NOT NULL,
    amount_cny NUMERIC(24,6) NOT NULL CHECK(amount_cny>=0),
    price_snapshot JSONB NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX sep_quotes_expiry_idx ON sep_quotes(expires_at);
