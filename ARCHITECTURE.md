# Architecture

The service is a Go 1.23 application assembled with Uber Fx. `cmd/wagering` composes configuration, PostgreSQL, OIDC, SQS, HTTP, reference retry, and outbox modules. PostgreSQL is the durable authority; SQS is an at-least-once transport. The `domain` package is independent of Fx, HTTP, SQS, and storage adapters.

Contents: [Lifecycle and shutdown](#lifecycle-and-shutdown) · [Money](#money) · [Wallet and ledger](#wallet-and-ledger) · [Transaction kinds and reversals](#transaction-kinds-and-reversals) · [Transaction state machine](#transaction-state-machine) · [Idempotency and the canonical hash](#idempotency-and-the-canonical-hash) · [Pending references](#pending-references) · [Error and failureCode catalog](#error-and-failurecode-catalog) · [HTTP contract](#http-contract) · [Authentication](#authentication-and-authorization) · [Inbound SQS](#inbound-sqs) · [Events and outbox](#events-and-outbox) · [Reconciliation](#reconciliation) · [Tests](#local-dependencies-and-tests) · [Limitations](#decisions-and-limitations)

## Lifecycle and shutdown

Fx constructs and starts dependencies before listeners and workers. `SHUTDOWN_TIMEOUT` (default `30s`; Compose and `.env.example` use `15s`) is applied as the Fx stop timeout through `bootstrap.NewWithConfig`, so it bounds the whole reverse-order shutdown on SIGINT/SIGTERM. Hooks stop in reverse start order: HTTP first (`http.Server.Shutdown` drains in-flight requests until the deadline), then the workers, and the PostgreSQL pool last. Database calls take the request or worker context. `cmd/migrate` provides explicit `up` and `down` commands; the app also applies pending migrations on startup (a migration whose content hash differs from the applied one aborts startup).

Worker behavior during shutdown differs by worker:

- **SQS consumer**: stops receiving and cancels its context. A message being processed is cancelled; its SQL transaction rolls back and its receipt is made visible again (visibility 0). Every unstarted receipt of a received batch is released the same way (D30). Receipts are never deleted on shutdown. If cancellation lands after the commit but before `DeleteMessage`, the message is redelivered and answered from the inbox as an idempotent replay.
- **Reference worker and outbox publisher**: stop claiming new rows. The row in flight finishes, bounded by `ItemTimeout` (reference) or `SendTimeout`+`AckTimeout` (outbox), or is cancelled when the stop deadline expires. A cancelled row keeps its lease and is taken over after the lease expires; a cancelled outbox send can therefore be published again with the same `eventId`.

## Money

`Money` is an immutable pair of signed `int64` minor units (cents) and an ISO 4217-shaped currency. There is no floating-point monetary arithmetic. The zero value is uninitialized and every operation rejects it.

External representation is the JSON object `{"amount":"12.34","currency":"BRL"}` with exactly these two string fields (unknown fields and non-string amounts are rejected).

- **Accepted amounts** (input): digits with an optional fractional part of one or two digits (`"10"`, `"10.5"`, `"10.50"`). Rejected: empty, a sign (so no negatives), exponent, `NaN`/`Infinity`, thousands separators, whitespace, a missing integer part (`".5"`), a trailing dot (`"5."`), more than two decimals (never rounded), and values above the range below.
- **Currency**: three ASCII letters, case-insensitive, normalized to upper case. Membership in the ISO 4217 registry is not checked.
- **Output**: amounts are always serialized with exactly two decimals (`"10.00"`).
- **Range**: external amounts are `0` to `9223372036854775807` minor units, that is `92233720368547758.07`. A larger value is rejected with `AMOUNT_OVERFLOW`. Addition, subtraction and negation are overflow-checked; a wallet credit that would exceed the `int64` range fails with `AMOUNT_OVERFLOW` instead of wrapping.
- **Per kind**: `BET`, `WIN`, `REFUND` and `ROLLBACK` need an amount greater than zero; `LOSS` needs exactly zero.

## Wallet and ledger

Wallet balances are nonnegative and each wallet has a monotonically increasing version. The schema enforces one wallet per `(player_id,currency)`, a nonnegative balance, unique transaction identities, valid ledger equations, and append-only ledger rows. Wallet-changing work runs in an explicit PostgreSQL `READ COMMITTED` transaction and locks the wallet row with `SELECT ... FOR UPDATE`. A balance change, transaction state, ledger entry, inbox completion when present, and outbox events commit atomically. Different wallets use different row locks. Read endpoints and reconciliation use PostgreSQL `REPEATABLE READ` snapshots.

Opening a wallet with a positive initial balance creates, in one commit, the wallet at version 1, an internal `OPENING` transaction (always `PROCESSED`, never accepted from HTTP/SQS), its credit ledger entry, and the `WagerTransactionProcessed` and `WalletBalanceChanged` events. A zero opening creates only the wallet. Opening the same `(playerId,currency)` again returns `409 CONFLICT`.

## Transaction kinds and reversals

| Kind | Amount | `referenceExternalTransactionId` | Effect |
| --- | --- | --- | --- |
| `BET` | > 0 | forbidden | debit; `INSUFFICIENT_FUNDS_BET` when the balance is short |
| `WIN` | > 0 | optional; when present it must name a `BET` | credit |
| `LOSS` | = 0 | forbidden | no ledger entry, no balance change, wallet version unchanged; the processed event is still emitted |
| `REFUND` | > 0, equal to the referenced amount | required, must name a `BET` | credit |
| `ROLLBACK` | > 0, equal to the referenced amount | required, must name a `BET`, `WIN` or `REFUND` | credit when it reverses a `BET`; debit when it reverses a `WIN` or `REFUND` (`INSUFFICIENT_FUNDS_REVERSAL` when the balance is short) |

Reference lookup is scoped by `providerId` and the reference external ID. Evaluation order for a found reference (`domain.EvaluateReference`):

1. Identity: a different provider, player or round gives `REFERENCE_MISMATCH`; a different wallet `WALLET_MISMATCH`; a different currency `CURRENCY_MISMATCH`. These are definitive even while the reference is still pending.
2. Kind pairing: `WIN`/`REFUND` may reference only a `BET`; `ROLLBACK` a `BET`, `WIN` or `REFUND`. Anything else (including a `ROLLBACK` of a `ROLLBACK` or of a `LOSS`) is `REFERENCE_MISMATCH`.
3. Amount: `REFUND` and `ROLLBACK` must equal the referenced amount exactly, otherwise `REFERENCE_MISMATCH` (no partial reversal).
4. Status: a `PENDING`/`PENDING_REFERENCE` reference means wait (see [Pending references](#pending-references)); a `REJECTED`/`FAILED` reference gives `REFERENCE_FAILED`; only a `PROCESSED` reference proceeds.
5. Reversal combinations (D07), checked under the wallet lock: a `BET` accepts at most one processed reversal in total, either a `REFUND` or a `ROLLBACK`, never both; a `WIN` or `REFUND` accepts at most one `ROLLBACK`. A violation gives `ALREADY_REVERSED`. A `ROLLBACK` of a `REFUND` does not reopen the `BET` for another refund (the `BET` is already reversed). A `WIN` that references a `BET` is not a reversal and does not consume it.

A unique index on processed reversals per referenced transaction is the second line of defense, so competing reversal requests cannot both succeed even across instances. Rejections are business results, not errors: the transaction ends `REJECTED` with a stable `failureCode`.

## Transaction state machine

```text
          +--> PROCESSED
PENDING --+--> PENDING_REFERENCE --+--> PROCESSED
          +--> REJECTED            +--> REJECTED
          +--> FAILED              +--> FAILED
```

`PROCESSED`, `REJECTED` and `FAILED` are terminal; a database trigger rejects changes to a terminal row. While waiting, `PENDING_REFERENCE` stays in that state and only its attempt counter and schedule change.

- `PENDING` is inserted and resolved inside the same SQL transaction, so a client or consumer observes only `PROCESSED`, `REJECTED` or `PENDING_REFERENCE`. The reference worker also claims `PENDING` rows, which only matters if such a row ever survives a crash.
- `PROCESSED` stores the resulting balance and wallet version; `REJECTED` stores the failure code (and the observed balance when it is meaningful); `PENDING_REFERENCE` stores the attempt count, next attempt time, reference deadline and lease.
- **Transient versus permanent.** A transient failure is an infrastructure error that may succeed later (connection refused, closed or exhausted pool, deadline exceeded, SQLSTATE class `08`, `57P0x`, `40001`, `40P01`, or a lost optimistic wallet update). It never finalizes a transaction: the SQL transaction rolls back, nothing is persisted, and the caller retries the same operation safely (HTTP `503` with `Retry-After`; SQS visibility backoff). A business rejection is a definitive, durable result. A permanent infrastructure outcome is modeled as `FAILED` with `PERMANENT_FAILURE`, but see the limitation below: no code path currently produces it.

## Idempotency and the canonical hash

HTTP requires an explicit `Idempotency-Key` header (nonblank, at most 255 bytes, no surrounding whitespace or control characters); the service never calculates a key for the caller. SQS carries the key as `data.idempotencyKey`. Identity is scoped by provider: `(providerId, idempotencyKey)` and `(providerId, externalTransactionId)` are each unique.

| Request | Result |
| --- | --- |
| New key and new external ID | processed once |
| Same key, same business content | the stored result is returned (`200`, `idempotentReplay: true`), including stored rejections and pending states |
| Same key, different content (different hash) | `409 HASH_MISMATCH` |
| Different key, external ID already used by the same provider | `409 CONFLICT` |

The check runs before and again after the wallet lock is taken, and a unique-constraint race on the key falls back to reading the committed result, so concurrent duplicates converge on one result.

**Canonical hash.** `request_hash` is the lowercase hex SHA-256 of a canonical JSON object. Keys are sorted lexicographically (Go `encoding/json` map ordering, including the nested `money` keys) and only business fields are included: `providerId`, `externalTransactionId`, `playerId`, `walletId`, `roundId`, `gameId`, `kind`, `money.amount`, `money.currency`, and `referenceExternalTransactionId` when present. Normalizations: the amount becomes two decimals (`"25"` and `"25.5"` hash as `"25.00"` and `"25.50"`), the currency upper case, the wallet UUID lower case with hyphens; opaque identifiers keep their exact spelling and `kind` is case-sensitive. The idempotency key, SQS `messageId`/`type`/`occurredAt`, HTTP headers and correlation IDs are excluded, so the same operation hashes identically over HTTP and SQS (`TestHTTPAndSQSShareCanonicalHash`).

The SQS inbox uses a different, simpler hash: the SHA-256 hex of the raw message body bytes, to detect the same `messageId` arriving with another body.

## Pending references

A `WIN` (with a reference), `REFUND` or `ROLLBACK` whose reference is missing, or exists but is still `PENDING`/`PENDING_REFERENCE`, moves to `PENDING_REFERENCE` (HTTP `202`) and one `WagerTransactionPendingReference` event is emitted on entry only. The reference worker (any number of instances) claims due rows with `FOR UPDATE SKIP LOCKED` and a lease, and applies each under the same wallet lock as a new request. The retry call itself requires a live lease owned by the caller on a due row; a stale or early attempt returns no-effect and does not consume the attempt budget (D50).

Defaults (`domain.DefaultRetryPolicy`, overridable with `REFERENCE_RETRY_*`; see the README configuration table):

- Backoff: 1 s, 2 s, 4 s, ... doubling up to a 5 minute cap.
- TTL: 24 hours from the transaction's creation. `REFERENCE_RETRY_MAX_ATTEMPTS` is 0 by default (no attempt cap); when set, it also ends the wait.
- Exhaustion is evaluated when an attempt runs, so the rejection is recorded by the first attempt at or after the TTL (or attempt cap). A missing reference then gives `REFERENCE_NOT_FOUND`; a reference that exists but never finalized gives `REFERENCE_UNRESOLVED`.
- If the reference resolves to `REJECTED`/`FAILED` the result is `REFERENCE_FAILED` immediately; identity, wallet, currency, kind and amount mismatches and `ALREADY_REVERSED` are rejected immediately, without waiting.
- A crash leaves a lease; another instance takes the row once the lease (default 60 s) expires.

## Error and failureCode catalog

There are two layers. A business rejection is a stored transaction result: it is returned in a transaction body (`status: "REJECTED"`, `failureCode`) and emitted as an event. Everything else is an error envelope `{"code","message","correlationId"}`.

### failureCode (persisted on the transaction)

| failureCode | Raised when | Definitive? |
| --- | --- | --- |
| `INSUFFICIENT_FUNDS_BET` | a `BET` debit exceeds the balance | yes |
| `INSUFFICIENT_FUNDS_REVERSAL` | a `ROLLBACK` of a `WIN`/`REFUND` would debit more than the balance | yes |
| `REFERENCE_NOT_FOUND` | the reference never appeared before TTL/attempt exhaustion | yes (after waiting) |
| `REFERENCE_UNRESOLVED` | the reference exists but was still pending at exhaustion | yes (after waiting) |
| `REFERENCE_FAILED` | the reference is `REJECTED` or `FAILED` | yes |
| `REFERENCE_MISMATCH` | provider, player or round differ; invalid kind pairing; `REFUND`/`ROLLBACK` amount differs from the reference | yes |
| `ALREADY_REVERSED` | the reference already has its allowed reversal | yes |
| `WALLET_MISMATCH` | `playerId` does not own `walletId`, or the reference is on another wallet | yes |
| `CURRENCY_MISMATCH` | the operation currency differs from the wallet's or the reference's | yes |
| `PERMANENT_FAILURE` | reserved for `FAILED` | see limitations |

### Correction semantics

- **Correctable, nothing recorded**: `400` validation errors, `401`, `403`, `404` and `409` conflicts persist nothing. Fix the request and resend; the same `Idempotency-Key` may be reused for a corrected `400`. A `409 HASH_MISMATCH` means the key already belongs to different content: resend the original content, or send the new content with a new key and external ID.
- **Retry unchanged**: `503 TEMPORARILY_UNAVAILABLE` finalized nothing. Retry the same key and body after `Retry-After` (5 s); idempotency guarantees at most one effect.
- **Wait**: `202` / `PENDING_REFERENCE` is not a failure; the service resolves it, and the same key returns the current state.
- **Definitive**: a `REJECTED` result is durable and final. Replaying its key or external ID returns the same rejection. To act differently, submit a new operation with a new `externalTransactionId` and `Idempotency-Key`.

### Error envelope codes

| code | HTTP | Meaning |
| --- | --- | --- |
| `INVALID_REQUEST` | 400 | malformed JSON, unknown fields, trailing values, invalid money, identifiers, UUIDs, kind, cursor or limit, missing/blank `Idempotency-Key` |
| domain validation codes (for example `AMOUNT_OVERFLOW`) | 400 | invalid input detected below the transport parser |
| `UNAUTHENTICATED` | 401 | missing, malformed, invalid or expired token (`WWW-Authenticate: Bearer`) |
| `FORBIDDEN` | 403 | wrong role/scope, or a provider acting for another provider |
| `NOT_FOUND` | 404 | unknown wallet/transaction, or another provider's transaction (existence is not revealed) |
| `HASH_MISMATCH` | 409 | idempotency key reused with different content |
| `CONFLICT` | 409 | external ID reused with another key; wallet already exists for player and currency |
| `INBOX_HASH_CONFLICT` | n/a | SQS only: `messageId` reused with a different body (poison, not an HTTP response) |
| `TEMPORARILY_UNAVAILABLE` | 503 | transient database/IdP-key failure or request cancellation; `Retry-After: 5` |
| `INTERNAL_ERROR` | 500 | unclassified failure; details are logged, not returned |
| `PERMANENT_FAILURE` | 500 | permanent infrastructure error carried by the domain error type |

## HTTP contract

| Status | When | Body |
| --- | --- | --- |
| `201` | new operation `PROCESSED`; wallet created | transaction (or wallet) |
| `202` | new operation `PENDING_REFERENCE` | transaction |
| `422` | new operation `REJECTED` | transaction with `status: "REJECTED"` and `failureCode` (not an error envelope) |
| `200` | idempotent replay of any stored result (`idempotentReplay: true`); reads, including reads of rejected transactions | transaction, wallet, ledger page or reconciliation |
| `400` / `401` / `403` / `404` / `409` / `500` / `503` | see the envelope table | error envelope |

The transaction body carries `transactionId`, `status`, `kind`, `walletId`, `playerId`, `amount`, `createdAt`, `updatedAt` and, when applicable, `providerId`, `externalTransactionId`, `roundId`, `gameId`, `referenceExternalTransactionId`, `failureCode`, `balance` (result balance), `walletVersion`, `idempotentReplay` and `completedAt`. `X-Correlation-ID` (up to 255 bytes) is echoed as `correlationId` in errors and logs; otherwise a UUID is generated. Request bodies are limited to 1 MiB. `GET /wallets/{id}/ledger` accepts `limit` (1 to 200, default 50) and an opaque `cursor` bound to the wallet; the response has `nextCursor`. `GET /health/ready` answers a plain-text `503 not ready` when PostgreSQL or the inbound queue is unreachable.

## Authentication and authorization

Provider tokens are validated against the configured OIDC issuer, audience (`wagering-api` in Compose), signature and expiry, with signing keys fetched from the JWKS endpoint (discovery, or `OIDC_JWKS_URL`). A provider token needs the `roles` claim `wagering:provider` and a `provider_id` claim (`OIDC_PROVIDER_CLAIM` overrides the claim name); an internal token needs `wagering:internal`. `provider_id` must match the `providerId` in a submitted body or path, otherwise `403`; lookups of another provider's records return `404`. If IdP keys cannot be fetched the service fails closed with `503`. There is no local password or token issuer. Local clients and secrets come from `infra/keycloak/realm-export.json`.

## Inbound SQS

`wager-transactions.fifo` is consumed by `internal/messaging/consumer`. Message body (strict JSON, one object, unknown fields rejected):

```json
{"messageId":"msg-123","type":"WagerTransactionRequested","occurredAt":"2026-09-08T12:00:00Z",
 "data":{"providerId":"provider-a","externalTransactionId":"transaction-123","playerId":"player-1",
         "walletId":"<wallet uuid>","roundId":"round-987","gameId":"fortune-chimp","kind":"BET",
         "money":{"amount":"25.00","currency":"BRL"},"idempotencyKey":"provider-a:transaction-123"}}
```

- **Identity.** `messageId` is the inbox identity `(consumer, messageId)`; `data.idempotencyKey` is the financial idempotency key. The consumer trusts `providerId` in the body: provider impersonation is prevented at the broker, not in the consumer (see [Authentication](#authentication-and-authorization) and the limitations).
- **Atomicity.** The inbox row, the financial effect, the ledger entry and the outbox events commit in one SQL transaction. The message is deleted only after that commit. A redelivered message with the same body is answered from the inbox and deleted.
- **Durable results are successes.** `PROCESSED`, `REJECTED` and `PENDING_REFERENCE` all delete the message; the reference worker resumes pending ones independently of the SQS receipt.
- **Invalid and poison messages.** A body that fails to parse or validate, an unsupported `type`, a `messageId` reused with another body (`INBOX_HASH_CONFLICT`), a key reused with other content (`HASH_MISMATCH`), an external ID reused with another key, or any other domain-invalid input is a permanent error. The consumer logs it, sets visibility to 1 s and never deletes it; SQS redrive moves it to `wager-transactions-dlq.fifo`.
- **Transient errors.** Every other error (database unavailable, serialization failure, an unknown wallet, any unclassified error) is treated as transient: visibility is set to 1 s, 2 s, 4 s, ... capped at 60 s according to `ApproximateReceiveCount`. An unknown wallet is therefore retried like an outage and reaches the DLQ only after the redrive budget.
- **Redrive.** `maxReceiveCount` is 15 (`infra/localstack/init-sqs.sh`, mirrored by `tests/integration/sqs`). Poison messages reach the DLQ in about 15 s; a valid message failing transiently while PostgreSQL still answers pings in about 10 minutes (`1+2+4+8+16+32+9x60 s`).
- **Outage pause.** Before every receive the consumer pings PostgreSQL (3 s bound). While the ping fails nothing is received, so an outage of any length spends none of the redrive budget; if a message of a batch fails transiently and the ping then fails, the remaining unstarted messages are released unprocessed. Messages in flight when the database fails cost one receive each.
- **Defaults.** Long poll 10 s, at most 10 messages per receive, visibility timeout 60 s (`consumer.DefaultConfig`).
- **FIFO ownership.** The application never sends to the inbound queue; the internal producer (`wagering-producer`, the only principal the queue policy allows to `SendMessage`) sets `MessageGroupId` and `MessageDeduplicationId`. The contract is `MessageGroupId=walletId` (parallelism across wallets, ordering inside one) and `MessageDeduplicationId=messageId` (D15); the queue also has content-based deduplication. The consumer does not read either value, and no correctness guarantee depends on them: the inbox, the idempotency key and the wallet lock provide it. HTTP and SQS operations on one wallet serialize on the wallet row lock, so ordering between them is not guaranteed (a reversal can arrive before its `BET` and waits as `PENDING_REFERENCE`). SQS FIFO deduplication lasts only five minutes. While a message is in flight or backing off, SQS FIFO holds later messages of the same group.
- **Broker access (D12, D45).** Local access goes through an Nginx gateway that forwards only requests carrying a provisioned IAM user's key, with identity and queue policies that let only the producer send to the inbound queue and give the service only consume/publish actions. MiniStack does not verify SigV4 signatures, so a local key id is a bearer credential; real SQS/IAM verifies signatures.

## Events and outbox

Events are immutable JSON snapshots in the transactional outbox, written in the same commit as the financial change. Envelope (`version` is `1`; there is no schema registry):

```json
{"eventId":"<uuid>","eventType":"WalletBalanceChanged","aggregateId":"<wallet uuid>",
 "correlationId":"...","causationId":"<transaction id, optional>",
 "occurredAt":"2026-09-08T12:00:00Z","version":1,"data":{ ... }}
```

Timestamps are UTC RFC3339 and money is `{"amount","currency"}`. `correlationId` is `X-Correlation-ID` (or a generated UUID) for HTTP, the `messageId` for SQS, and the transaction ID for reference retries.

| `eventType` | Emitted | `data` |
| --- | --- | --- |
| `WagerTransactionProcessed` | a transaction reaches `PROCESSED` (including `LOSS` and `OPENING`) | `transactionId`, `walletId`, `playerId`, `kind`, `providerId`, `externalTransactionId`, `roundId`, `gameId`, `referenceExternalTransactionId` (external fields omitted for `OPENING`), `money`, `resultBalance` |
| `WagerTransactionRejected` | a transaction reaches `REJECTED` | the same transaction fields, `failureCode`, `observedBalance` (omitted for `WALLET_MISMATCH` and `CURRENCY_MISMATCH`) |
| `WagerTransactionPendingReference` | first entry to `PENDING_REFERENCE` only | the same transaction fields, `attemptCount`, `nextAttemptAt` |
| `WalletBalanceChanged` | every ledger entry (not `LOSS`) | `walletId`, `transactionId`, `direction`, `money`, `balanceBefore`, `balanceAfter`, `walletVersion` |

A processed ledger operation emits `WagerTransactionProcessed` then `WalletBalanceChanged`; a rejection emits one event. `LOSS` emits only the processed event.

**Routing.** All event types go to one FIFO queue, `wager-events.fifo` (`EVENT_QUEUE_URL`). Each message has `MessageGroupId=aggregateId` (the wallet), `MessageDeduplicationId=eventId`, and string message attributes `eventId` and `eventType`; the body is the stored envelope. Only the `wagering-app` identity may send to it; no downstream consumer identity is provisioned, so local inspection uses `wagering-broker-operator`. The events queue has no DLQ. Consumers must deduplicate by `eventId`: a crash after broker acceptance but before the database records publication, or an expired lease, republishes the same event, and FIFO deduplication covers only five minutes.

**Publishing and ordering.** Publishers claim rows with `FOR UPDATE SKIP LOCKED` and a lease (default 30 s), and any number of instances may run. The claim query only admits an aggregate's earliest unpublished event, so events of one wallet are published strictly in outbox order even across instances (D31, D40), while other wallets proceed independently. After a successful publish the publisher polls again immediately instead of waiting for the poll interval, so the next event of that wallet is claimed at once. A send failure schedules a retry with exponential backoff (1 s up to 5 minutes), recorded with `attempt_count` and `last_error`. Acknowledgement is guarded by lease owner and expiry. Tunable through `OUTBOX_*` (README).

## Reconciliation

`POST /wallets/{id}/reconciliation` reads the wallet and its ledger in one `REPEATABLE READ` snapshot, computes the balance from ledger effects, and reports stored balance, calculated balance, difference, `consistent` and the entry count. It never mutates financial state; divergences are logged and counted in `wagering_reconciliation_divergences_total`.

## Local dependencies and tests

Docker Compose provisions PostgreSQL, Keycloak identities, and the SQS-compatible queues behind the signed gateway. See [README.md](README.md) for bootstrap, tokens, migrations and commands.

Unit tests cover domain transitions, amount/hash boundaries and error mapping (`internal/domain`, `internal/idempotency`, `internal/httpapi`). PostgreSQL-backed integration tests cover schema and constraints, transaction operations, retries and outbox claims (`internal/storage`, `internal/workers`). `tests/integration/auth` and `tests/integration/http` run real Keycloak flows and verify access control and read contracts against a scratch database. `tests/integration/sqs` verifies the provisioned redrive policy, poison dead-lettering, transient retry, producer/gateway access rules and PostgreSQL outage survival against the real broker. `tests/system` builds and launches three independent application processes against real PostgreSQL, Keycloak and SQS and exercises duplicates, the 80/80 race, restart, commit-before-delete redelivery, pending-reference resumption, outbox lease recovery and reconciliation. Its crash hooks require a separate `-tags=systemfault` build and are absent from normal production binaries (D36).

## Decisions and limitations

- Database row-level locking and uniqueness constraints are the serialization boundary; there is no process-global financial lock.
- PostgreSQL is a single primary database; horizontal service instances are supported, sharding is not.
- SQS and the local SQS-compatible implementation are at-least-once. Stable IDs plus durable inbox/outbox records make repeats safe; they do not create a distributed exactly-once transaction.
- **Outbox poison event.** The outbox has no attempt cap, no parking table and no skip tool. An event that can never be published (a permanently rejected body, or a stored payload that fails the header check, which is retried each time its lease expires) is retried forever and blocks every later event of its wallet, by design, to keep per-wallet order. Other wallets are unaffected. It is visible as `last_error`/`attempt_count` on the row and in `wagering_outbox_unpublished_count` and `wagering_outbox_oldest_unpublished_age_milliseconds`. Recovery is to fix the cause (broker, queue policy, deployment); the head event then publishes at its next scheduled retry (up to 5 minutes later) and the backlog drains in order. The snapshot is immutable and unpublished rows cannot be deleted by the database guard; marking an event published by hand would drop it from downstream, and no such procedure is provided or tested.
- **`FAILED` is not produced.** The state machine, schema and `PERMANENT_FAILURE` code exist, but no production path moves a transaction to `FAILED`. Reference expiry yields `REJECTED`; permanent SQS errors are dead-lettered and permanent HTTP errors return an error without a stored transaction.
- **Residual SQS window.** If PostgreSQL answers pings but commits keep failing for longer than about 10 minutes, a valid wager is dead-lettered and needs a manual redrive from the DLQ (`start-message-move-task` or re-sending the body). An unknown wallet over SQS takes the same path.
- The inbound consumer trusts `providerId` in the message body; protection against impersonation is the broker policy, which locally is enforced by the gateway/key id and not by signature verification (D45).
- Event consumers receive at-least-once delivery and must deduplicate by `eventId`; the events queue has no DLQ.
- Keycloak and broker credentials in this repository are for local development only. Production requires managed credentials, TLS and production IdP/queue policy.
- JWT revocation is not immediate: locally verified signed tokens remain acceptable until expiry unless the issuer rotates keys or changes validation policy.
