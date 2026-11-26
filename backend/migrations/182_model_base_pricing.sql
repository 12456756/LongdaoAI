CREATE TABLE upstream_companies (
    id BIGSERIAL PRIMARY KEY,
    name VARCHAR(100) NOT NULL UNIQUE CHECK (length(trim(name)) > 0),
    notes TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE accounts ADD COLUMN upstream_company_id BIGINT REFERENCES upstream_companies(id);
CREATE INDEX accounts_upstream_company_idx ON accounts(upstream_company_id);

CREATE TABLE account_model_prices (
    id BIGSERIAL PRIMARY KEY,
    account_id BIGINT NOT NULL REFERENCES accounts(id),
    model VARCHAR(200) NOT NULL,
    revision BIGINT NOT NULL DEFAULT 1,
    active BOOLEAN NOT NULL DEFAULT true,
    price JSONB NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(account_id, model)
);

CREATE TABLE model_base_prices (
    model VARCHAR(200) PRIMARY KEY,
    source_price_id BIGINT NOT NULL REFERENCES account_model_prices(id),
    source_revision BIGINT NOT NULL,
    version BIGINT NOT NULL DEFAULT 1,
    price JSONB NOT NULL,
    updated_by BIGINT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE model_base_price_history (
    id BIGSERIAL PRIMARY KEY,
    model VARCHAR(200) NOT NULL,
    version BIGINT NOT NULL,
    source_price_id BIGINT NOT NULL,
    source_revision BIGINT NOT NULL,
    price JSONB NOT NULL,
    updated_by BIGINT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(model, version)
);

CREATE TABLE request_model_price_snapshots (
    api_key_id BIGINT NOT NULL,
    user_id BIGINT NOT NULL,
    request_id VARCHAR(300) NOT NULL,
    model VARCHAR(200) NOT NULL,
    price JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY(api_key_id,request_id)
);
