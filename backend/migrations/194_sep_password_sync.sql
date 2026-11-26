ALTER TABLE sep_enterprises ADD COLUMN IF NOT EXISTS password_sync_version BIGINT NOT NULL DEFAULT 0;
ALTER TABLE sep_enterprises ADD COLUMN IF NOT EXISTS password_sync_ref TEXT NOT NULL DEFAULT '';
ALTER TABLE sep_enterprises ADD COLUMN IF NOT EXISTS password_sync_hash TEXT NOT NULL DEFAULT '';
