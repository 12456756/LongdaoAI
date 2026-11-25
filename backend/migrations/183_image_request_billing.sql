CREATE TABLE IF NOT EXISTS image_request_billing (
    api_key_id BIGINT NOT NULL,
    request_id VARCHAR(320) NOT NULL,
    user_id BIGINT NOT NULL,
    fingerprint VARCHAR(64) NOT NULL,
    model TEXT NOT NULL,
    image_size VARCHAR(32) NOT NULL,
    status VARCHAR(20) NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'settled', 'not_charged')),
    actual_cost NUMERIC(24,12),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (api_key_id, request_id),
    CHECK ((status = 'pending' AND actual_cost IS NULL)
        OR (status = 'not_charged' AND actual_cost = 0 AND actual_cost IS NOT NULL)
        OR (status = 'settled' AND actual_cost >= 0 AND actual_cost IS NOT NULL))
);
