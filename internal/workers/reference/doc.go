// Package reference is the durable worker that resumes external operations
// whose referenced transaction was missing or unfinished (REQ-023, REQ-042,
// REQ-043, REQ-044).
//
// The pending state lives only in PostgreSQL: wager_transactions rows in
// PENDING or PENDING_REFERENCE carry next_attempt_at, an attempt counter, a
// reference deadline and a lease. The worker holds no in-memory queue, so a
// crash or restart loses nothing: a fresh process simply claims the same rows
// when they fall due.
//
// One pass is: claim due rows under a lease (FOR UPDATE SKIP LOCKED, so two
// workers never receive the same row), then hand each id to the shared
// financial use case (wagering.Service.RetryPending). That use case re-reads the
// row under the wallet lock and either applies the operation, records another
// attempt with exponential backoff, or rejects it with REFERENCE_NOT_FOUND /
// REFERENCE_UNRESOLVED once the attempt cap or the TTL is exhausted, together
// with the WagerTransactionRejected outbox event. The use case first verifies,
// under the locks and on the database clock, that the caller still owns an
// unexpired lease and that next_attempt_at is due; a worker whose lease expired
// and was taken over (or that never claimed the row) is refused with
// wagering.ErrClaimNotHeld before any write, so it cannot consume the attempt
// budget, reschedule or reject early. A row that an earlier worker already
// finished is observed as terminal and left untouched, so a lease that expired
// mid-flight cannot double-apply an effect.
package reference
