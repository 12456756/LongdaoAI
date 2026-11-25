-- Financial evidence is durable; task routing and submission data remain in Redis.
CREATE TABLE IF NOT EXISTS video_request_billing (
    api_key_id BIGINT NOT NULL,
    request_id VARCHAR(320) NOT NULL,
    user_id BIGINT NOT NULL,
    fingerprint VARCHAR(64) NOT NULL,
    model TEXT NOT NULL,
    resolution VARCHAR(10) NOT NULL,
    duration_seconds INTEGER NOT NULL,
    status VARCHAR(20) NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'settled', 'not_charged')),
    actual_cost NUMERIC(24,12),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (api_key_id, request_id),
    CHECK ((status = 'pending' AND actual_cost IS NULL)
        OR (status = 'not_charged' AND actual_cost IS NOT NULL AND actual_cost = 0)
        OR (status = 'settled' AND actual_cost IS NOT NULL AND actual_cost >= 0))
);
