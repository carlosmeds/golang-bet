# Architecture

The service is a Go 1.23 application assembled with Uber Fx. `cmd/wagering` composes configuration, PostgreSQL, OIDC, SQS, HTTP, reference retry, and outbox modules. PostgreSQL is the durable authority; SQS is an at-least-once transport. The `domain` package is independent of Fx, HTTP, SQS, and storage adapters.

## Lifecycle and dependencies

Fx constructs and starts dependencies before listeners and workers. Shutdown runs hooks in reverse order: HTTP stops accepting requests, workers stop claiming new work and finish or cancel bounded in-flight operations, and PostgreSQL closes last. Database operations accept request/worker contexts. `cmd/migrate` provides explicit `up` and `down` commands; the app also applies pending migrations on startup.

## Money, wallet and ledger

Money stores signed integer minor units and an ISO 4217 currency. External JSON uses a decimal string with exactly two fractional digits; parsing rejects exponent notation, excess scale, negative external values, and overflow. There is no floating-point monetary arithmetic. Wallet balances are nonnegative and each wallet has a monotonically increasing version.

The schema enforces one wallet per `(player_id,currency)`, a nonnegative balance, unique transaction identities, valid ledger equations, and append-only ledger rows. Wallet-changing work runs in an explicit PostgreSQL `READ COMMITTED` transaction and locks the wallet row with `SELECT ... FOR UPDATE`. A balance change, transaction state, ledger entry, inbox completion when present, and outbox events commit atomically. Different wallets use different row locks. Read endpoints and reconciliation use PostgreSQL `REPEATABLE READ` snapshots.

## Operations and idempotency

The shared wagering use case serves HTTP and SQS. A deterministic canonical JSON hash covers business fields and normalized amounts, while excluding idempotency key and transport metadata. Reuse of the same key and same content returns the persisted result; different content conflicts. HTTP requires an explicit `Idempotency-Key`. Provider/external transaction ID is separately unique so a second key cannot apply the same external transaction.

BET debits, WIN credits, LOSS records success without a ledger balance mutation, REFUND reverses exactly one processed BET, and ROLLBACK reverses one processed BET/WIN/REFUND. Reference resolution scopes by provider and validates player, wallet, currency and round. Unique constraints prevent double reversal across competing requests. A reversal debit that cannot be funded is durably rejected with its own failure code. Opening creates a stable internal OPENING transaction, credit ledger and events atomically for positive initial balances; zero opening creates no financial records.

Pending transactions and `PENDING_REFERENCE` rows are durable. The reference worker claims due rows with leases, bounded exponential backoff and expiry/max attempts. A process crash leaves a lease that another instance can take after expiry. Rejections and terminal outcomes have stable failure codes.

## Inbox, outbox and SQS

The inbound `wager-transactions.fifo` consumer uses the SQS `messageId` as inbox identity and stores a payload hash. Inbox completion and financial writes share one SQL transaction. A message is deleted only after durable commit. A matching duplicate is harmless; a conflicting body for the same message ID is detected. Business rejections are durable results; transient failures are retried with bounded exponential visibility backoff (1 s doubling to 60 s) while the consumer pauses receiving whenever the PostgreSQL ping fails, so an outage does not spend the redrive budget (`maxReceiveCount` 15; see README "Retry and redrive bounds"); malformed/permanent messages reach the configured DLQ through SQS redrive. The default long poll is 10 seconds, batch size is at most 10, and visibility timeout is 60 seconds. Shutdown releases unstarted receipts and lets in-flight work finish within the application shutdown deadline.

Events are immutable snapshots in the transactional outbox. Publishers claim rows using database leases and `SKIP LOCKED`; multiple instances can publish concurrently. Events for an aggregate retain sequence order. Each SQS FIFO event uses aggregate ID as `MessageGroupId` and stable `eventId` as `MessageDeduplicationId`. If publish succeeds but the database acknowledgment is lost, the same event ID can be published again; downstream consumers must deduplicate. Lease expiry recovers abandoned work and retry delay is bounded.

Typed event envelopes carry event ID/type, aggregate, correlation and optional causation IDs, occurrence time, version, and typed data. Timestamps are UTC RFC3339 and money is serialized as decimal strings. Events cover processed operations (including LOSS), rejections, balance changes, and pending-reference transitions.

## HTTP and OIDC

HTTP exposes wallet creation/read/ledger/reconciliation and wagering submit/read routes. Provider tokens are validated against configured OIDC issuer, audience, signature and expiration; `provider_id` scopes submissions and reads. Internal-service scope protects wallet operations. Cross-provider lookups return not found to avoid revealing another provider's records. There is no local password or token issuer.

Reconciliation reads wallet and ledger from one repeatable-read snapshot, calculates opening plus ledger effects and reports stored balance, calculated balance, difference, consistency and entry count. It does not mutate financial state.

## Local dependencies and tests

Docker Compose provisions PostgreSQL, Keycloak identities, and SQS-compatible inbound/outbound FIFO queues with a redrive DLQ. Local Keycloak client secrets and endpoint examples are development-only. See [README.md](README.md) for bootstrap, tokens, migrations and commands.

Unit tests cover domain transitions and amount/hash boundaries. PostgreSQL-backed integration tests cover schema and constraints, transaction operations, retries and outbox claims. `tests/integration/auth` and `tests/integration/http` run real Keycloak token flows and verify access control using a PostgreSQL scratch database. `tests/system` builds and launches three independent application processes against real PostgreSQL, Keycloak and SQS, then tests the duplicate/race cases, cross-transport redelivery after commit-before-delete, process restart, pending-reference resumption, competing outbox recovery and ledger reconciliation. Its crash hooks require a separate `-tags=systemfault` build and are absent from normal production binaries. The system suite runs against the Compose stack (see README.md); `tests/integration/sqs` verifies the provisioned redrive policy, poison-message dead-lettering and transient retry against the real SQS-compatible broker.

## Decisions and limitations

- Database row-level locking and uniqueness constraints are the serialization boundary; the service does not use a process-global financial lock.
- PostgreSQL remains a single primary database; horizontal service instances are supported, database sharding is not part of this implementation.
- SQS and the local SQS-compatible implementation are at-least-once. Stable IDs plus durable inbox/outbox records make repeats safe; they do not create a distributed exactly-once transaction.
- Keycloak/local stack credentials are only for local development. Production requires managed credentials, TLS and production IdP/queue policy.
- JWT revocation is not immediate: locally verified signed tokens remain acceptable until expiry unless the issuer rotates keys or changes validation policy.
