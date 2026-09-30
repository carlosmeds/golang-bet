-- Cross-table invariants checked at COMMIT (deferred constraint triggers), so the
-- application may write wallet, transaction and ledger in any order inside one
-- SQL transaction, but can never commit them inconsistent:
--   * a wallet balance/version change has the matching ledger entry,
--   * every ledger entry belongs to a PROCESSED transaction and to the wallet state,
--   * every PROCESSED non-LOSS transaction has its ledger entry and result balance.

CREATE FUNCTION wallets_require_ledger() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    entry ledger_entries%ROWTYPE;
BEGIN
    -- A wallet that was opened empty has neither ledger entry nor OPENING transaction.
    IF NEW.version = 1 AND NEW.balance_amount = 0 THEN
        RETURN NULL;
    END IF;

    SELECT * INTO entry FROM ledger_entries
     WHERE wallet_id = NEW.id AND wallet_version = NEW.version;
    IF NOT FOUND OR entry.balance_after <> NEW.balance_amount THEN
        RAISE EXCEPTION 'wallet % (version %, balance %) has no matching ledger entry',
            NEW.id, NEW.version, NEW.balance_amount
            USING ERRCODE = 'check_violation', CONSTRAINT = 'wallets_ledger_match';
    END IF;
    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER wallets_require_ledger_commit
    AFTER INSERT OR UPDATE ON wallets
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION wallets_require_ledger();

CREATE FUNCTION ledger_entries_require_state() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    w  wallets%ROWTYPE;
    tx wager_transactions%ROWTYPE;
BEGIN
    SELECT * INTO w FROM wallets WHERE id = NEW.wallet_id;
    -- Later entries of the same commit are validated by their own trigger.
    IF w.version < NEW.wallet_version
       OR (w.version = NEW.wallet_version AND w.balance_amount <> NEW.balance_after) THEN
        RAISE EXCEPTION 'ledger entry % is not reflected in wallet % (version %, balance %)',
            NEW.id, w.id, w.version, w.balance_amount
            USING ERRCODE = 'check_violation', CONSTRAINT = 'ledger_entries_wallet_match';
    END IF;

    SELECT * INTO tx FROM wager_transactions WHERE id = NEW.transaction_id;
    IF tx.status <> 'PROCESSED' THEN
        RAISE EXCEPTION 'ledger entry % belongs to transaction % in status %', NEW.id, tx.id, tx.status
            USING ERRCODE = 'check_violation', CONSTRAINT = 'ledger_entries_transaction_processed';
    END IF;
    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER ledger_entries_require_state_commit
    AFTER INSERT ON ledger_entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION ledger_entries_require_state();

CREATE FUNCTION wager_transactions_require_ledger() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    entry ledger_entries%ROWTYPE;
BEGIN
    IF NEW.status <> 'PROCESSED' OR NEW.kind = 'LOSS' THEN
        RETURN NULL;
    END IF;

    SELECT * INTO entry FROM ledger_entries
     WHERE wallet_id = NEW.wallet_id AND transaction_id = NEW.id;
    IF NOT FOUND
       OR NEW.result_balance_amount IS DISTINCT FROM entry.balance_after
       OR NEW.result_balance_currency IS DISTINCT FROM entry.currency
       OR (NEW.result_wallet_version IS NOT NULL AND NEW.result_wallet_version <> entry.wallet_version) THEN
        RAISE EXCEPTION 'processed % transaction % has no ledger entry matching its result', NEW.kind, NEW.id
            USING ERRCODE = 'check_violation', CONSTRAINT = 'wager_transactions_ledger_match';
    END IF;
    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER wager_transactions_require_ledger_commit
    AFTER INSERT OR UPDATE ON wager_transactions
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION wager_transactions_require_ledger();
