// Package outbox publishes the transactional outbox to the outbound event
// queue (REQ-033, REQ-068, REQ-069, REQ-072).
//
// Events are written to outbox_events inside the financial SQL transaction, so
// nothing is published before commit. This worker then claims due rows with
// SKIP LOCKED and a lease, sends each stored JSON envelope unchanged (the jsonb snapshot, identical on every publication), and marks
// the row published. The eventId never changes, so a crash between send and
// acknowledgement, or a lease that expires mid-publish, produces a repeat
// publication with the same eventId; consumers deduplicate on it (D11). Any
// number of publisher instances may run against one database.
package outbox
