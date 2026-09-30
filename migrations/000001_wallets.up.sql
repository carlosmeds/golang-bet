-- Wallets: one balance per (player, currency). The balance can never be negative
-- and the version only moves together with a balance change.

CREATE TABLE wallets (
    id             uuid        NOT NULL,
    player_id      text        NOT NULL,
    currency       char(3)     NOT NULL,
    balance_amount bigint      NOT NULL DEFAULT 0,
    version        bigint      NOT NULL DEFAULT 1,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT wallets_pkey PRIMARY KEY (id),
    -- One wallet per (playerId, currency); a second opening is a conflict.
    CONSTRAINT wallets_player_currency_key UNIQUE (player_id, currency),
    -- Target of composite foreign keys that must agree on the wallet currency.
    CONSTRAINT wallets_id_currency_key UNIQUE (id, currency),
    CONSTRAINT wallets_player_id_check CHECK (char_length(btrim(player_id)) BETWEEN 1 AND 255),
    CONSTRAINT wallets_currency_check CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT wallets_balance_nonnegative_check CHECK (balance_amount >= 0),
    CONSTRAINT wallets_version_positive_check CHECK (version >= 1)
);

-- Guards row-level rules that a CHECK cannot express (it cannot compare OLD and NEW).
CREATE FUNCTION wallets_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.version <> 1 THEN
            RAISE EXCEPTION 'wallet must be created with version 1 (got %)', NEW.version
                USING ERRCODE = 'check_violation', CONSTRAINT = 'wallets_initial_version';
        END IF;
        RETURN NEW;
    END IF;

    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'wallets cannot be deleted'
            USING ERRCODE = 'restrict_violation', CONSTRAINT = 'wallets_no_delete';
    END IF;

    IF NEW.id IS DISTINCT FROM OLD.id
       OR NEW.player_id IS DISTINCT FROM OLD.player_id
       OR NEW.currency IS DISTINCT FROM OLD.currency
       OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'wallet identity columns are immutable'
            USING ERRCODE = 'restrict_violation', CONSTRAINT = 'wallets_immutable_identity';
    END IF;

    -- Version increases by exactly one on a balance change and never otherwise.
    IF NEW.balance_amount <> OLD.balance_amount THEN
        IF NEW.version <> OLD.version + 1 THEN
            RAISE EXCEPTION 'balance change must bump version from % to %, got %',
                OLD.version, OLD.version + 1, NEW.version
                USING ERRCODE = 'check_violation', CONSTRAINT = 'wallets_version_step';
        END IF;
    ELSIF NEW.version <> OLD.version THEN
        RAISE EXCEPTION 'version may only change together with the balance'
            USING ERRCODE = 'check_violation', CONSTRAINT = 'wallets_version_step';
    END IF;

    NEW.updated_at := now();
    RETURN NEW;
END;
$$;

CREATE TRIGGER wallets_guard_write
    BEFORE INSERT OR UPDATE ON wallets
    FOR EACH ROW EXECUTE FUNCTION wallets_guard();

CREATE TRIGGER wallets_guard_delete
    BEFORE DELETE ON wallets
    FOR EACH ROW EXECUTE FUNCTION wallets_guard();
