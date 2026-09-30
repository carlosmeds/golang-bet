DROP TRIGGER IF EXISTS wallets_guard_delete ON wallets;
DROP TRIGGER IF EXISTS wallets_guard_write ON wallets;
DROP FUNCTION IF EXISTS wallets_guard();
DROP TABLE IF EXISTS wallets;
