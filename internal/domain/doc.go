// Package domain holds the wagering domain model: Money, Wallet,
// WagerTransaction, ledger entries, typed events and the inbox/outbox records.
//
// The package depends only on the Go standard library (no Fx, HTTP, SQS or
// persistence libraries) and performs no I/O. Constructors enforce invariants
// for new values; Rehydrate* functions rebuild values from stored state and
// enforce the same invariants, so a corrupted row cannot become a domain value.
// Identifiers and clocks are always supplied by callers to keep the model
// deterministic and testable.
package domain
