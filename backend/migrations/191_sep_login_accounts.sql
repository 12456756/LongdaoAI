-- Login credentials can change independently of the stable enterprise binding.
CREATE OR REPLACE FUNCTION sep_protect_user() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS(SELECT 1 FROM sep_enterprises WHERE user_id=OLD.id) THEN
        IF TG_OP='DELETE' THEN RAISE EXCEPTION 'SEP_ACCOUNT_MANAGED'; END IF;
        IF NEW.balance IS DISTINCT FROM OLD.balance OR NEW.frozen_balance IS DISTINCT FROM OLD.frozen_balance
           OR NEW.role IS DISTINCT FROM OLD.role OR NEW.deleted_at IS DISTINCT FROM OLD.deleted_at THEN
            RAISE EXCEPTION 'SEP_ACCOUNT_MANAGED: use SEP ledger operations';
        END IF;
    END IF;
    RETURN NEW;
END $$;
