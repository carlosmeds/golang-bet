DROP TRIGGER IF EXISTS ledger_entries_validate_insert ON ledger_entries;
DROP TRIGGER IF EXISTS ledger_entries_no_truncate ON ledger_entries;
DROP TRIGGER IF EXISTS ledger_entries_no_update_delete ON ledger_entries;
DROP TABLE IF EXISTS ledger_entries;
DROP FUNCTION IF EXISTS ledger_entries_validate();
DROP FUNCTION IF EXISTS ledger_entries_append_only();
