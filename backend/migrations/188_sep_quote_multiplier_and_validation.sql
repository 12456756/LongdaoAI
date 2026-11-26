ALTER TABLE sep_quotes ADD COLUMN quoted_multiplier NUMERIC(24,12) NOT NULL DEFAULT 1;
CREATE TABLE sep_validation_keys (
    integration_id VARCHAR(100) NOT NULL,
    kind VARCHAR(10) NOT NULL CHECK(kind IN ('image','video')),
    user_id BIGINT NOT NULL REFERENCES users(id),
    api_key_id BIGINT NOT NULL UNIQUE REFERENCES api_keys(id),
    PRIMARY KEY(integration_id,kind)
);
