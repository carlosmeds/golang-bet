# Mandatory requirement traceability

`SPEC.md` remains the authoritative local input. This index gives workers only the requirements relevant to their assigned task. The Lead updates the evidence column as commits and tests land. IDs are stable; do not renumber them.

| ID | Requirement | Tasks | Verification / evidence target |
| --- | --- | --- | --- |
| REQ-001 | Use Go; pin the same version in `go.mod` and Dockerfile; commit `go.mod` and `go.sum`. | T01,T12,T19 | Compare versions; clean build. |
| REQ-002 | Compose configuration, connections, repositories, use cases, handlers and workers with Uber Fx modules, constructors and invokes. | T01,T19 | Fx integration startup test. |
| REQ-003 | Fx lifecycle validates dependencies and coordinates graceful HTTP/worker stop, cancellation, deadlines and resource closing order. | T01,T09,T10,T11,T15 | Shutdown integration test. |
| REQ-004 | Domain must be independent of Fx, HTTP, SQS and persistence libraries. | T03,T19 | Package dependency review. |
| REQ-005 | Use PostgreSQL, local AWS SQS and Docker Compose with real dependencies in integration tests. | T02,T12,T15 | Compose and integration suite. |
| REQ-006 | Provide versioned reversible migrations and document both directions. | T02,T18 | Migration up/down test. |
| REQ-007 | Use SQL transactions, locks and constraints that are explicit and inspectable; document chosen library and transaction boundary. | T02,T06,T18 | SQL review and concurrency test. |
| REQ-008 | All I/O operations accept context and respect cancellation/timeout. | T01,T05,T06,T08,T09,T10,T11 | Cancellation tests/review. |
| REQ-009 | Never use floating point for money in parsing, calculation, serialization or storage. | T03,T04,T06,T19 | Static review and Money tests. |
| REQ-010 | Money is immutable, contains amount and ISO 4217 currency, and supports parse, zero, add, subtract, negate, compare and serialize. | T03,T14 | Unit tests. |
| REQ-011 | External Money JSON uses decimal string and currency; fixed two decimals, no silent rounding. | T03,T04,T14 | JSON/parse vectors. |
| REQ-012 | Reject empty, NaN, Infinity, exponent, excess scale and negative external amounts; reject uninitialized domain values. | T03,T04,T14 | Invalid input tests. |
| REQ-013 | Money arithmetic checks currency equality and int64 overflow in parse, add, subtract and negate. | T03,T14 | Boundary unit tests. |
| REQ-014 | Internal signed differences are allowed; wallet balance remains nonnegative. | T03,T02,T14 | Unit and SQL constraint tests. |
| REQ-015 | Wallet encapsulates ID, player, currency, balance, version and timestamps; creation and rehydration are distinct. | T03,T14 | Domain tests. |
| REQ-016 | One wallet per `(playerId,currency)`; opening duplicates return conflict. | T02,T07,T08,T15 | Unique constraint and HTTP tests. |
| REQ-017 | Wallet debit/credit enforce currency and nonnegative balance and require matching ledger in same SQL commit. | T03,T06,T07,T15 | Atomicity tests. |
| REQ-018 | Wallet version starts at 1 and increases only on post-creation balance change. | T03,T07,T15 | Opening, LOSS, credit/debit tests. |
| REQ-019 | Database prevents lost updates and permits independent wallets to progress concurrently; no global lock. | T02,T06,T07,T17 | Three-process and parallel-wallet tests. |
| REQ-020 | WagerTransaction records all applicable external identity, hash, reference, state, failure, result and timestamps. | T02,T03,T06 | Schema/domain review and replay tests. |
| REQ-021 | WagerTransaction uses PENDING, PENDING_REFERENCE, PROCESSED, REJECTED, FAILED with validated transitions; terminals cannot transition. | T03,T07,T14 | State-machine tests. |
| REQ-022 | Distinguish transient infrastructure errors from permanent audited failures and durable business rejection. | T03,T07,T09,T10,T14 | Error mapping and retry tests. |
| REQ-023 | Every committed PENDING/PENDING_REFERENCE has durable takeover after crash or restart. | T06,T07,T10,T17 | Kill/restart test. |
| REQ-024 | OPENING is internal only; external HTTP/SQS OPENING is rejected. | T02,T03,T04,T07,T08,T09,T14 | Contract/schema tests. |
| REQ-025 | OPENING has stable internal identity and excludes inapplicable external fields; duplicate initial credit is impossible. | T02,T03,T07,T15 | Constraint and opening tests. |
| REQ-026 | Positive wallet opening atomically creates PROCESSED OPENING, credit ledger, processed and balance events; version is 1. | T03,T06,T07,T15 | Opening integration test. |
| REQ-027 | Zero wallet opening creates no OPENING, ledger or financial event. | T03,T07,T15 | Opening integration test. |
| REQ-028 | Ledger entry records wallet/transaction IDs, direction, amount, before/after balances and timestamp, validating arithmetic. | T02,T03,T06,T14 | Unit and DB tests. |
| REQ-029 | Ledger is append-only, unique by `(walletId,transactionId)`, protected from update/delete by DB. | T02,T15 | Constraint/trigger tests. |
| REQ-030 | LOSS and rejected operations do not create ledger entries; LOSS does not change wallet version. | T03,T07,T14,T15 | Domain/integration tests. |
| REQ-031 | Inbox stores consumer/message identity, hash, received/completed times and unique pair. | T02,T06,T09,T15 | Schema/replay tests. |
| REQ-032 | SQS inbox completion and applicable domain, ledger and outbox effects commit in one SQL transaction. | T06,T07,T09,T15 | Crash-window test. |
| REQ-033 | Outbox stores stable event identity, aggregate, type, immutable payload snapshot, occurrence, attempts, next send and publication state. | T02,T03,T06,T11,T15 | Schema and event tests. |
| REQ-034 | BET debits positive amount and rejects insufficient balance. | T03,T07,T14,T17 | Unit and 80/80 race test. |
| REQ-035 | WIN credits positive amount; optional BET reference must be from same round and compatible identity. | T03,T07,T14,T15 | Reference tests. |
| REQ-036 | LOSS requires amount zero and matching currency; succeeds without ledger/balance event but emits processed event. | T03,T07,T14,T15 | LOSS tests. |
| REQ-037 | REFUND credits exactly a processed BET amount and requires referenceExternalTransactionId. | T03,T07,T14,T15 | Reversal tests. |
| REQ-038 | ROLLBACK reverses exactly one processed BET, WIN or REFUND and requires referenceExternalTransactionId. | T03,T07,T14,T15 | Reversal tests. |
| REQ-039 | Reference lookup uses `(providerId,referenceExternalTransactionId)`; provider, player, wallet, currency and round must match. | T06,T07,T14,T15 | Mismatch tests. |
| REQ-040 | Prevent two successful reversals of the same kind and double refund of one BET across REFUND/ROLLBACK. | T02,T06,T07,T17 | Concurrent reversal test. |
| REQ-041 | Reversal debit with insufficient balance is audited and has distinct code from BET insufficient funds. | T03,T07,T14,T15 | Failure-code tests. |
| REQ-042 | Missing or pending reference persists PENDING_REFERENCE with exponential retry across restart. | T02,T07,T10,T17 | Out-of-order/restart test. |
| REQ-043 | Reference retry has max attempts or TTL; exhaustion rejects with stable code and rejection event. | T10,T15,T17 | Expiration test. |
| REQ-044 | Existing rejected/failed reference is handled definitively and documented. | T07,T10,T14,T18 | Reference failure tests. |
| REQ-045 | All business rejections expose documented stable failureCode values, distinguishing recoverable input errors and definitive results. | T03,T04,T07,T08,T18 | Error contract tests. |
| REQ-046 | Durable idempotency handles repeats across HTTP/SQS and process restarts. | T02,T04,T06,T07,T09,T17 | Cross-transport/restart tests. |
| REQ-047 | HTTP Idempotency-Key is required and never silently replaced with a calculated key. | T04,T08,T16 | HTTP contract tests. |
| REQ-048 | Deterministic canonical JSON hash covers business fields only; excludes key/transport metadata; normalization is documented. | T04,T14,T18 | Cross-transport hash vectors. |
| REQ-049 | Same key/same payload replays persisted result with idempotentReplay true; changed payload conflicts. | T04,T06,T07,T08,T14 | Replay/conflict tests. |
| REQ-050 | Same `(providerId,externalTransactionId)` cannot be applied with a second key. | T02,T06,T07,T15 | Unique conflict test. |
| REQ-051 | Completed replay returns balance observed at original processing, despite later wallet changes. | T02,T06,T07,T08,T15 | Replay-after-change test. |
| REQ-052 | Provide POST /wallets and GET wallet, ledger, transaction by internal ID and provider/external ID, POST wagering and reconciliation. | T08,T16 | HTTP integration suite. |
| REQ-053 | Ledger pagination uses opaque cursor and stable order. | T08,T16 | Pagination test. |
| REQ-054 | Transaction reads expose pending state and failure codes. | T08,T16 | HTTP read tests. |
| REQ-055 | HTTP contract distinguishes invalid input, conflict, business rejection, pending and transient unavailability via codes and bodies. | T04,T08,T18 | Contract tests. |
| REQ-056 | Reconciliation uses consistent snapshot, reconstructs ledger including OPENING, reports stored/calculated/difference/consistent/count and never mutates balance. | T06,T08,T15 | Concurrent reconciliation test. |
| REQ-057 | Reconciliation divergence appears in response, JSON log and metric. | T08,T13,T15 | Divergence test. |
| REQ-058 | Public liveness and DB/SQS readiness endpoints. | T08,T13,T15 | Health tests. |
| REQ-059 | Business endpoints require external OIDC authentication; no local password/token issuer. | T05,T08,T12,T16 | Real IdP integration. |
| REQ-060 | Validate missing, invalid and expired credentials; use authenticated identity to authorize providerId. | T05,T08,T16 | Auth matrix. |
| REQ-061 | Provider reads and replays reveal only own transactions; wallet operations are internal-service only; unauthorized requests have no financial effects. | T05,T08,T16 | Cross-provider/security tests. |
| REQ-062 | SQS access is controlled by broker credentials/policy and consumer still validates domain input. | T09,T12,T15 | Queue policy and invalid-message tests. |
| REQ-063 | Provision inbound wager-transactions.fifo and wager-transactions-dlq.fifo with redrive. | T12,T15 | Queue discovery and redrive test. |
| REQ-064 | Consumer uses envelope messageId for durable inbox identity and compares hash on reentry. | T09,T15 | Duplicate/conflict test. |
| REQ-065 | Delete SQS message only after durable commit; business rejection can be deleted; transient error retries and permanent/exhausted goes DLQ. | T09,T15,T17 | Crash/retry/DLQ tests. |
| REQ-066 | Document visibility timeout, retry bounds, invalid messages and graceful SIGTERM visibility release. | T09,T18 | Shutdown test and docs. |
| REQ-067 | Document FIFO grouping/dedup IDs and test concurrent HTTP/SQS inputs. | T09,T12,T17,T18 | Cross-transport race test. |
| REQ-068 | Outbox publisher is separate, supports multiple publishers, claim contention, backoff and abandoned-work recovery. | T06,T11,T15,T17 | Two-publisher crash tests. |
| REQ-069 | Publish only after SQL commit; repeat publish preserves eventId. | T07,T11,T15,T17 | Before/after publish crash tests. |
| REQ-070 | Provision outbound event destination and document routing/consumption. | T11,T12,T18 | Queue discovery/event receipt. |
| REQ-071 | Emit processed (including LOSS), rejected, balance changed and pending-reference events at specified triggers. | T03,T07,T10,T15 | Event matrix tests. |
| REQ-072 | Typed event envelopes include eventId, eventType, aggregateId, correlationId, optional causationId, occurredAt, version and typed data. | T03,T04,T11,T14 | Serialization tests. |
| REQ-073 | WalletBalanceChanged contains walletId, transactionId, direction, money, before/after balance and walletVersion. | T03,T07,T14 | Payload tests. |
| REQ-074 | Event constructors set type/version; UTC RFC3339 timestamps and decimal money strings. | T03,T04,T14 | Serialization tests. |
| REQ-075 | JSON logs include available correlation/message/transaction/wallet/provider IDs without credentials or full financial payloads. | T13,T19 | Log inspection tests. |
| REQ-076 | Metrics cover status, duplicates, retries, DLQ, conflicts, outbox lag, processing latency and reconciliation divergence. | T13,T15 | Metrics assertions. |
| REQ-077 | Unit tests cover Money, wallet, state machine, all operation types, zero policy, opening and hash collision. | T14 | `go test ./...`. |
| REQ-078 | Integration tests use real PostgreSQL, IdP and SQS, including migrations, constraints, auth, inbox/outbox, retry and restart. | T15,T16 | Container integration command. |
| REQ-079 | Test Fx composition, start/stop and worker resource release. | T01,T15 | Lifecycle integration test. |
| REQ-080 | Test 50 concurrent duplicate BETs, 80/80 race on 100 balance, independent wallets and at least three separate instances. | T17 | Multi-process scenario. |
| REQ-081 | Test crash after consumer commit before delete, publisher before/after publication, pending reference arrival/expiry and app restart. | T17 | Fault-injection scenario. |
| REQ-082 | Test HTTP/SQS duplicate crossing with actual repeated deliveries and final ledger-derived balance. | T17 | Cross-transport scenario. |
| REQ-083 | Run applicable `go test -race ./...` and `go vet ./...`; format with gofmt. | T19 | Final command logs. |
| REQ-084 | README documents clean bootstrap, env, queues, migrations up/down, auth examples and all test/integration/multi-instance/fault commands. | T18,T19 | Clean-checkout documentation audit. |
| REQ-085 | `.env.example` has local examples and no real secrets; IdP auto-provisions identities. | T12,T18 | Compose/auth setup check. |
| REQ-086 | ARCHITECTURE documents money, transactions, idempotency, locks, pending references, reversals, inbox/outbox, auth, Fx, shutdown, interpretations and limitations. | T18,T19 | Documentation audit. |

## Current evidence snapshot

This is an implementation checkpoint, not a completion claim. The verification target column above remains authoritative for final acceptance.

- REQ-058: `/health/live` and `/health/ready` handlers now exist; readiness pings PostgreSQL and checks inbound SQS attributes. Live checks still need Compose verification.
- REQ-057, REQ-075, REQ-076: reconciliation divergence logging/metrics and current safe identifiers are present. T19 verified that request-body IDs are missing from wagering request logs and that outcome/source counters, worker processing latency, unpublished outbox count/age and actual DLQ arrivals are not covered. These remain open under R19-F5 and R19-F10C; do not treat the earlier metrics snapshot as final evidence.
- REQ-006: embedded up/down migration runner and `cmd/migrate` exist; package compiles. Real reversal cycle still needs PostgreSQL verification in this sandbox.
- REQ-068/069/070: outbox claim/order/retry/publish implementation and PostgreSQL tests are present; live crash and ordering tests pass with `-race` after aligning assertions to one-head-per-aggregate claims.
- REQ-052/059/060/061: auth and HTTP integration suites pass with live Keycloak/PostgreSQL under `-race`; repair R18 updates the harness to current SQS/metrics dependencies.
- REQ-065/067/068/069/080/081/082: live T17 passed under `-race` four consecutive times with three independent processes, isolated live SQS FIFO queues, HTTP/SQS idempotency crossing, inbox commit-before-delete crash and redelivery, pending-reference restart and resolution, outbox lease recovery, and ledger-derived balance reconciliation. T19 found the additional concurrent HTTP/SQS race, wallet-lock independence and post-80/80 replay assertions still need direct evidence (R19-F7).
- T19's full requirement matrix and evidence are in `.agent/handoffs/T19.md`. Until repairs land, REQ-062 is **Fail**; REQ-003, 019, 022, 045, 055, 065, 066, 067, 070, 076 and 086 have the partial gaps listed in findings F-2 through F-8; the lower-severity findings affect REQ-059/060, REQ-031/032/064/065, REQ-042/043 and REQ-075. Repair tasks are recorded in `.agent/TASKS.yaml` and Orca Run `run_21967f35dd24`.
- Baseline `go test ./...`, `go test -race ./...`, `go vet ./...`, T17 and sequential live integration evidence passed before post-review repairs. The final gates must be rerun after every repair has been integrated.
