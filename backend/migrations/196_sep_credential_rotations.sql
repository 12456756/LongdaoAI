ALTER TABLE sep_credentials DROP CONSTRAINT sep_credentials_enterprise_pk_kind_key;
ALTER TABLE sep_credentials ADD COLUMN is_current BOOLEAN NOT NULL DEFAULT TRUE;
ALTER TABLE sep_credentials ADD COLUMN generation_state VARCHAR(16) NOT NULL DEFAULT 'active'
    CHECK (generation_state IN ('active','draining','read_only'));
ALTER TABLE sep_credentials ADD CONSTRAINT sep_credentials_version_key UNIQUE(enterprise_pk,kind,version);
CREATE UNIQUE INDEX sep_credentials_current_idx ON sep_credentials(enterprise_pk,kind) WHERE is_current;

CREATE TABLE sep_credential_rotations (
    integration_id VARCHAR(100) NOT NULL,
    operation_id VARCHAR(128) NOT NULL,
    enterprise_pk BIGINT NOT NULL REFERENCES sep_enterprises(id),
    kind VARCHAR(10) NOT NULL CHECK(kind IN ('chat','image','video')),
    old_credential_id VARCHAR(64) NOT NULL REFERENCES sep_credentials(id),
    new_credential_id VARCHAR(64) NOT NULL REFERENCES sep_credentials(id),
    request_hash CHAR(64) NOT NULL,
    status VARCHAR(16) NOT NULL DEFAULT 'prepared' CHECK(status IN ('prepared','acknowledged','completed')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY(integration_id,operation_id)
);
CREATE UNIQUE INDEX sep_credential_rotations_open_idx ON sep_credential_rotations(enterprise_pk,kind) WHERE status<>'completed';
