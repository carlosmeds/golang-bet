// Package schema names the PostgreSQL objects created by the migrations so that
// repositories can classify constraint violations without string literals of
// their own. schema_test.go verifies that every name below exists in the SQL.
package schema

// Tables.
const (
	TableWallets           = "wallets"
	TableWagerTransactions = "wager_transactions"
	TableLedgerEntries     = "ledger_entries"
	TableInboxMessages     = "inbox_messages"
	TableOutboxEvents      = "outbox_events"
)

// SQLSTATE codes raised by the schema.
const (
	SQLStateNotNullViolation    = "23502"
	SQLStateForeignKeyViolation = "23503"
	SQLStateUniqueViolation     = "23505"
	SQLStateCheckViolation      = "23514"
	// SQLStateRestrictViolation is raised by the immutability triggers: ledger
	// update/delete/truncate, changes to terminal transactions, wallet deletion,
	// changes to outbox snapshots and inbox identity.
	SQLStateRestrictViolation = "23001"
)

// Unique and primary key constraints a repository maps to domain conflicts.
const (
	// One wallet per (player, currency): opening conflict.
	UniqueWalletPlayerCurrency = "wallets_player_currency_key"
	// Same provider reused an idempotency key for a different external transaction.
	UniqueTransactionIdempotencyKey = "wager_transactions_provider_idempotency_key"
	// Same (provider, externalTransactionId) submitted under a second key.
	UniqueTransactionExternalID = "wager_transactions_provider_external_key"
	// A wallet has at most one OPENING; the stable internal key also collides
	// first, so a duplicate opening reports either of these two.
	UniqueTransactionOpening     = "wager_transactions_opening_wallet_key"
	UniqueTransactionInternalKey = "wager_transactions_internal_key_key"
	// A referenced transaction has at most one processed REFUND/ROLLBACK.
	UniqueProcessedReversal = "wager_transactions_processed_reversal_key"
	// Ledger: one entry per (wallet, transaction) and per wallet version.
	UniqueLedgerWalletTransaction = "ledger_entries_wallet_transaction_key"
	UniqueLedgerWalletVersion     = "ledger_entries_wallet_version_key"
	// Inbox identity is (consumer, messageId).
	UniqueInboxMessage = "inbox_messages_pkey"
	// Outbox event identity.
	UniqueOutboxEvent = "outbox_events_pkey"
)

// Constraints and triggers raising SQLSTATE 23514 or 23001 that signal a
// programming error (the application must have prevented them).
const (
	CheckWalletBalanceNonNegative  = "wallets_balance_nonnegative_check"
	CheckWalletVersionStep         = "wallets_version_step"
	CheckWalletLedgerMatch         = "wallets_ledger_match"
	CheckLedgerEquation            = "ledger_entries_equation_check"
	CheckLedgerChain               = "ledger_entries_chain"
	CheckLedgerTransactionMatch    = "ledger_entries_transaction_match"
	CheckLedgerWalletMatch         = "ledger_entries_wallet_match"
	CheckTransactionLedgerMatch    = "wager_transactions_ledger_match"
	CheckTransactionReferenceMatch = "wager_transactions_reference_compatible"
	RestrictLedgerAppendOnly       = "ledger_entries_append_only"
	RestrictTransactionTerminal    = "wager_transactions_terminal"
)
