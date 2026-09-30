DROP TRIGGER IF EXISTS wager_transactions_guard_delete ON wager_transactions;
DROP TRIGGER IF EXISTS wager_transactions_guard_write ON wager_transactions;
DROP FUNCTION IF EXISTS wager_transactions_guard();
DROP TABLE IF EXISTS wager_transactions;
