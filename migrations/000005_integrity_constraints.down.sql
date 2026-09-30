DROP TRIGGER IF EXISTS wager_transactions_require_ledger_commit ON wager_transactions;
DROP FUNCTION IF EXISTS wager_transactions_require_ledger();
DROP TRIGGER IF EXISTS ledger_entries_require_state_commit ON ledger_entries;
DROP FUNCTION IF EXISTS ledger_entries_require_state();
DROP TRIGGER IF EXISTS wallets_require_ledger_commit ON wallets;
DROP FUNCTION IF EXISTS wallets_require_ledger();
