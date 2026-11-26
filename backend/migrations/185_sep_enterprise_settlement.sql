CREATE TABLE sep_enterprises (
    id BIGSERIAL PRIMARY KEY,
    integration_id VARCHAR(100) NOT NULL,
    enterprise_id VARCHAR(128) NOT NULL,
    registration_ref VARCHAR(128) NOT NULL,
    registration_hash CHAR(64) NOT NULL,
    user_id BIGINT NOT NULL UNIQUE REFERENCES users(id),
    company_name VARCHAR(200) NOT NULL,
    admin_email VARCHAR(254) NOT NULL,
    status VARCHAR(20) NOT NULL DEFAULT 'ready' CHECK (status IN ('ready','disabled')),
    available_cny NUMERIC(24,6) NOT NULL DEFAULT 0 CHECK (available_cny >= 0),
    frozen_cny NUMERIC(24,6) NOT NULL DEFAULT 0 CHECK (frozen_cny >= 0),
    sequence BIGINT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(integration_id,enterprise_id),
    UNIQUE(integration_id,registration_ref)
);
CREATE TABLE sep_credentials (
    id VARCHAR(64) PRIMARY KEY,
    enterprise_pk BIGINT NOT NULL REFERENCES sep_enterprises(id),
    kind VARCHAR(10) NOT NULL CHECK (kind IN ('chat','image','video')),
    api_key_id BIGINT NOT NULL UNIQUE REFERENCES api_keys(id),
    version INTEGER NOT NULL DEFAULT 1,
    encryption_key_id VARCHAR(100) NOT NULL,
    masked_key VARCHAR(64) NOT NULL,
    encrypted_key TEXT NOT NULL,
    UNIQUE(enterprise_pk,kind)
);
CREATE TABLE sep_executions (
    id VARCHAR(64) PRIMARY KEY,
    integration_id VARCHAR(100) NOT NULL,
    enterprise_pk BIGINT NOT NULL REFERENCES sep_enterprises(id),
    execution_id VARCHAR(128) NOT NULL,
    api_key_id BIGINT NOT NULL REFERENCES api_keys(id),
    kind VARCHAR(10) NOT NULL,
    model VARCHAR(200) NOT NULL,
    request_hash CHAR(64) NOT NULL,
    execution_status VARCHAR(20) NOT NULL DEFAULT 'received',
    fee_status VARCHAR(20) NOT NULL DEFAULT 'pending',
    resource_version BIGINT NOT NULL DEFAULT 1,
    exchange_rate NUMERIC(24,12) NOT NULL CHECK (exchange_rate>0),
    price_snapshot JSONB NOT NULL,
    multiplier_snapshot JSONB NOT NULL,
    provider_request_id TEXT,
    task_id TEXT,
    account_id BIGINT REFERENCES accounts(id),
    usage JSONB,
    gross_usd NUMERIC(24,12),
    gross_cny NUMERIC(24,6) CHECK (gross_cny>=0),
    bill_id VARCHAR(64) UNIQUE,
    bill_version INTEGER,
    fee_evidence_hash CHAR(64),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    finalized_at TIMESTAMPTZ,
    UNIQUE(integration_id,execution_id),
    CHECK ((fee_status='final') = (gross_cny IS NOT NULL AND bill_id IS NOT NULL))
);
CREATE INDEX sep_executions_enterprise_idx ON sep_executions(enterprise_pk,created_at DESC);
CREATE INDEX sep_executions_task_idx ON sep_executions(enterprise_pk,task_id) WHERE task_id IS NOT NULL;
CREATE TABLE sep_settlements (
    integration_id VARCHAR(100) NOT NULL,
    settlement_id VARCHAR(128) NOT NULL,
    execution_pk VARCHAR(64) NOT NULL UNIQUE REFERENCES sep_executions(id),
    request_hash CHAR(64) NOT NULL,
    gross_cny NUMERIC(24,6) NOT NULL,
    free_cny NUMERIC(24,6) NOT NULL CHECK (free_cny>=0),
    paid_cny NUMERIC(24,6) NOT NULL CHECK (paid_cny>=0),
    receipt JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY(integration_id,settlement_id),
    CHECK (gross_cny=free_cny+paid_cny)
);
CREATE TABLE sep_transfers (
    integration_id VARCHAR(100) NOT NULL,
    transfer_id VARCHAR(128) NOT NULL,
    enterprise_pk BIGINT NOT NULL REFERENCES sep_enterprises(id),
    request_hash CHAR(64) NOT NULL,
    receipt JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY(integration_id,transfer_id)
);
CREATE TABLE sep_ledger (
    id VARCHAR(64) PRIMARY KEY,
    enterprise_pk BIGINT NOT NULL REFERENCES sep_enterprises(id),
    sequence BIGINT NOT NULL,
    type VARCHAR(20) NOT NULL CHECK (type IN ('credit_compute','debit_compute','settlement')),
    amount_cny NUMERIC(24,6) NOT NULL CHECK (amount_cny>=0),
    free_cny NUMERIC(24,6) NOT NULL DEFAULT 0 CHECK (free_cny>=0),
    available_cny NUMERIC(24,6) NOT NULL CHECK (available_cny>=0),
    frozen_cny NUMERIC(24,6) NOT NULL CHECK (frozen_cny>=0),
    transfer_id VARCHAR(128),
    execution_id VARCHAR(128),
    settlement_id VARCHAR(128),
    effective_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(enterprise_pk,sequence)
);
CREATE TABLE sep_request_nonces (
    integration_id VARCHAR(100) NOT NULL,
    nonce VARCHAR(128) NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY(integration_id,nonce)
);
CREATE INDEX sep_request_nonces_expiry_idx ON sep_request_nonces(expires_at);

-- Integration users cannot acquire a second, unsynchronized USD funding path.
CREATE FUNCTION sep_protect_user() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS(SELECT 1 FROM sep_enterprises WHERE user_id=OLD.id) THEN
        IF TG_OP='DELETE' THEN RAISE EXCEPTION 'SEP_ACCOUNT_MANAGED'; END IF;
        IF NEW.balance IS DISTINCT FROM OLD.balance OR NEW.frozen_balance IS DISTINCT FROM OLD.frozen_balance
           OR NEW.password_hash IS DISTINCT FROM OLD.password_hash OR NEW.email IS DISTINCT FROM OLD.email
           OR NEW.role IS DISTINCT FROM OLD.role OR NEW.deleted_at IS DISTINCT FROM OLD.deleted_at THEN
            RAISE EXCEPTION 'SEP_ACCOUNT_MANAGED: use SEP ledger operations';
        END IF;
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER sep_protect_user BEFORE UPDATE OR DELETE ON users FOR EACH ROW EXECUTE FUNCTION sep_protect_user();

CREATE FUNCTION sep_protect_key() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE owner_id BIGINT;
BEGIN
    IF TG_OP='INSERT' THEN owner_id:=NEW.user_id; ELSE owner_id:=OLD.user_id; END IF;
    IF EXISTS(SELECT 1 FROM sep_enterprises WHERE user_id=owner_id)
       AND COALESCE(current_setting('sep.key_management',true),'')<>'on' THEN
        RAISE EXCEPTION 'SEP_KEY_MANAGED: use SEP key operations';
    END IF;
    IF TG_OP='DELETE' THEN RETURN OLD; END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER sep_protect_key BEFORE INSERT OR UPDATE OF key,user_id,group_id,quota,rate_limit_5h,rate_limit_1d,rate_limit_7d OR DELETE ON api_keys
    FOR EACH ROW EXECUTE FUNCTION sep_protect_key();
