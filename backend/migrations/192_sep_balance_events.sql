ALTER TABLE sep_ledger ADD COLUMN origin VARCHAR(16) NOT NULL DEFAULT 'sep';
ALTER TABLE sep_ledger ADD COLUMN business_ref TEXT;
ALTER TABLE sep_ledger ADD COLUMN notes TEXT NOT NULL DEFAULT '';
ALTER TABLE sep_ledger ADD COLUMN actor_id BIGINT;
ALTER TABLE sep_ledger ADD COLUMN exchange_rate NUMERIC(24,12);
ALTER TABLE sep_ledger DROP CONSTRAINT sep_ledger_type_check;
ALTER TABLE sep_ledger ADD CONSTRAINT sep_ledger_type_check CHECK (type IN
    ('credit_compute','debit_compute','settlement','paid_consumption','free_record','compute_credit','compute_debit','compute_refund'));
ALTER TABLE sep_executions ADD COLUMN billing_origin VARCHAR(16) NOT NULL DEFAULT 'sep';
ALTER TABLE sep_executions ADD COLUMN billing_error TEXT NOT NULL DEFAULT '';

CREATE TABLE sep_balance_operations (
    user_id BIGINT NOT NULL REFERENCES users(id),
    business_ref VARCHAR(200) NOT NULL,
    request_hash CHAR(64) NOT NULL,
    receipt JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY(user_id,business_ref)
);
CREATE TABLE sep_balance_events (
    event_id VARCHAR(64) PRIMARY KEY REFERENCES sep_ledger(id),
    enterprise_pk BIGINT NOT NULL REFERENCES sep_enterprises(id),
    sequence BIGINT NOT NULL,
    payload JSONB NOT NULL,
    status VARCHAR(16) NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','retrying','delivered','failed')),
    attempts INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_error TEXT NOT NULL DEFAULT '',
    delivered_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(enterprise_pk,sequence)
);
CREATE INDEX sep_balance_events_pending_idx ON sep_balance_events(next_attempt_at) WHERE status<>'delivered';

CREATE TABLE sep_payment_refunds (
    order_id BIGINT PRIMARY KEY REFERENCES payment_orders(id),
    attempt INTEGER NOT NULL,
    amount_cny NUMERIC(24,6) NOT NULL,
    status VARCHAR(16) NOT NULL CHECK(status IN ('deducted','restored','completed'))
);

-- The ledger insert and its delivery record always commit or roll back together.
CREATE FUNCTION sep_record_balance_event() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE ent sep_enterprises%ROWTYPE; delta NUMERIC(24,6);
BEGIN
    SELECT * INTO STRICT ent FROM sep_enterprises WHERE id=NEW.enterprise_pk;
    delta:=CASE WHEN NEW.type IN ('credit_compute','compute_credit') THEN NEW.amount_cny
                WHEN NEW.type='free_record' THEN 0 ELSE -NEW.amount_cny END;
    INSERT INTO sep_balance_events(event_id,enterprise_pk,sequence,payload) VALUES(NEW.id,NEW.enterprise_pk,NEW.sequence,
        jsonb_build_object('contract_version',1,'event_id',NEW.id,'enterprise_id',ent.enterprise_id,
        'compute_account_id',ent.user_id,'sequence',NEW.sequence::text,'origin',NEW.origin,'type',NEW.type,
        'currency','CNY','delta_cny',delta::text,'amount_cny',NEW.amount_cny::text,'free_cny',NEW.free_cny::text,
        'available_paid_cny',NEW.available_cny::text,'frozen_paid_cny',NEW.frozen_cny::text,
        'business_ref',NEW.business_ref,'transfer_id',NEW.transfer_id,'execution_id',NEW.execution_id,
        'settlement_id',NEW.settlement_id,'exchange_rate',NEW.exchange_rate::text,
        'occurred_at',NEW.effective_at));
    RETURN NEW;
END $$;
CREATE TRIGGER sep_record_balance_event AFTER INSERT ON sep_ledger FOR EACH ROW EXECUTE FUNCTION sep_record_balance_event();
