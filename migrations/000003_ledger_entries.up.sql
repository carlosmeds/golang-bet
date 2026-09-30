-- Ledger: append-only record of every balance movement. Each entry carries the
-- wallet version it produced, so a wallet's entries form a gap-free chain
-- (wallet_version is also the stable per-wallet pagination order).

CREATE TABLE ledger_entries (
    id              uuid        NOT NULL,
    wallet_id       uuid        NOT NULL,
    transaction_id  uuid        NOT NULL,
    wallet_version  bigint      NOT NULL,
    direction       text        NOT NULL,
    amount          bigint      NOT NULL,
    currency        char(3)     NOT NULL,
    balance_before  bigint      NOT NULL,
    balance_after   bigint      NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT ledger_entries_pkey PRIMARY KEY (id),
    -- Wallet and currency must agree with the wallet row.
    CONSTRAINT ledger_entries_wallet_fkey
        FOREIGN KEY (wallet_id, currency) REFERENCES wallets (id, currency) ON DELETE RESTRICT,
    -- The transaction must belong to the same wallet.
    CONSTRAINT ledger_entries_transaction_fkey
        FOREIGN KEY (transaction_id, wallet_id) REFERENCES wager_transactions (id, wallet_id) ON DELETE RESTRICT,
    -- One entry per (wallet, transaction) and one entry per wallet version.
    CONSTRAINT ledger_entries_wallet_transaction_key UNIQUE (wallet_id, transaction_id),
    CONSTRAINT ledger_entries_wallet_version_key UNIQUE (wallet_id, wallet_version),
    CONSTRAINT ledger_entries_direction_check CHECK (direction IN ('CREDIT', 'DEBIT')),
    CONSTRAINT ledger_entries_currency_check CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT ledger_entries_version_check CHECK (wallet_version >= 1),
    CONSTRAINT ledger_entries_amount_positive_check CHECK (amount > 0),
    CONSTRAINT ledger_entries_balances_nonnegative_check CHECK (balance_before >= 0 AND balance_after >= 0),
    -- The ledger equation.
    CONSTRAINT ledger_entries_equation_check CHECK (
        (direction = 'CREDIT' AND balance_after = balance_before + amount)
        OR (direction = 'DEBIT' AND balance_after = balance_before - amount)
    )
);

CREATE FUNCTION ledger_entries_append_only() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'ledger_entries is append-only (% rejected)', TG_OP
        USING ERRCODE = 'restrict_violation', CONSTRAINT = 'ledger_entries_append_only';
END;
$$;

CREATE TRIGGER ledger_entries_no_update_delete
    BEFORE UPDATE OR DELETE ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_entries_append_only();

CREATE TRIGGER ledger_entries_no_truncate
    BEFORE TRUNCATE ON ledger_entries
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_entries_append_only();

-- Entries must extend the wallet chain and agree with the transaction they record.
CREATE FUNCTION ledger_entries_validate() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    tx   wager_transactions%ROWTYPE;
    prev ledger_entries%ROWTYPE;
BEGIN
    SELECT * INTO tx FROM wager_transactions WHERE id = NEW.transaction_id;

    IF tx.kind = 'LOSS' THEN
        RAISE EXCEPTION 'LOSS transactions never create ledger entries'
            USING ERRCODE = 'check_violation', CONSTRAINT = 'ledger_entries_kind';
    END IF;
    IF tx.amount <> NEW.amount OR tx.currency <> NEW.currency
       OR (tx.kind = 'BET' AND NEW.direction <> 'DEBIT')
       OR (tx.kind IN ('OPENING', 'WIN', 'REFUND') AND NEW.direction <> 'CREDIT') THEN
        RAISE EXCEPTION 'ledger entry does not match transaction % (%)', tx.id, tx.kind
            USING ERRCODE = 'check_violation', CONSTRAINT = 'ledger_entries_transaction_match';
    END IF;
    -- A ROLLBACK moves money the opposite way of what it reverses.
    IF tx.kind = 'ROLLBACK' THEN
        IF (tx.reference_kind = 'BET' AND NEW.direction <> 'CREDIT')
           OR (tx.reference_kind IN ('WIN', 'REFUND') AND NEW.direction <> 'DEBIT')
           OR tx.reference_kind IS NULL THEN
            RAISE EXCEPTION 'ROLLBACK ledger direction does not reverse its reference'
                USING ERRCODE = 'check_violation', CONSTRAINT = 'ledger_entries_transaction_match';
        END IF;
    END IF;

    -- Only OPENING produces version 1.
    IF (tx.kind = 'OPENING') <> (NEW.wallet_version = 1) THEN
        RAISE EXCEPTION 'only the OPENING entry may produce wallet version 1'
            USING ERRCODE = 'check_violation', CONSTRAINT = 'ledger_entries_opening_version';
    END IF;

    -- Entries are appended strictly in wallet-version order.
    IF EXISTS (SELECT 1 FROM ledger_entries
                WHERE wallet_id = NEW.wallet_id AND wallet_version >= NEW.wallet_version) THEN
        RAISE EXCEPTION 'ledger entries must be appended in wallet-version order'
            USING ERRCODE = 'check_violation', CONSTRAINT = 'ledger_entries_chain';
    END IF;

    SELECT * INTO prev FROM ledger_entries
     WHERE wallet_id = NEW.wallet_id AND wallet_version = NEW.wallet_version - 1;
    IF FOUND THEN
        IF prev.balance_after <> NEW.balance_before THEN
            RAISE EXCEPTION 'balance_before % does not continue previous balance_after %',
                NEW.balance_before, prev.balance_after
                USING ERRCODE = 'check_violation', CONSTRAINT = 'ledger_entries_chain';
        END IF;
    ELSIF NEW.wallet_version = 1 THEN
        IF NEW.balance_before <> 0 THEN
            RAISE EXCEPTION 'opening entry must start from zero'
                USING ERRCODE = 'check_violation', CONSTRAINT = 'ledger_entries_chain';
        END IF;
    ELSIF NEW.wallet_version = 2 THEN
        -- Zero-balance opening creates no ledger entry: first movement starts from zero.
        IF NEW.balance_before <> 0 THEN
            RAISE EXCEPTION 'first movement of a zero-opened wallet must start from zero'
                USING ERRCODE = 'check_violation', CONSTRAINT = 'ledger_entries_chain';
        END IF;
    ELSE
        RAISE EXCEPTION 'ledger chain gap: no entry for wallet version %', NEW.wallet_version - 1
            USING ERRCODE = 'check_violation', CONSTRAINT = 'ledger_entries_chain';
    END IF;

    RETURN NEW;
END;
$$;

CREATE TRIGGER ledger_entries_validate_insert
    BEFORE INSERT ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_entries_validate();
