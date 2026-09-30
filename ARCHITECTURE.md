# Architecture

The Wagering module is designed as a distributed, idempotent, event-driven ledger. It guarantees consistency for financial operations across multiple transports (HTTP and SQS) and concurrent instances.

## Application Architecture (Fx & Shutdown)

The application relies on `go.uber.org/fx` for dependency injection and lifecycle management. Modules encapsulate specific domain boundaries (e.g., Auth, Storage, HTTP). `bootstrap.New()` wires these components and ensures clean shutdown. During SIGTERM, Fx gracefully stops HTTP listeners, aborts SQL connections, and safely releases in-flight SQS message visibility timeouts so they can be immediately retried.

## Money & Arithmetic

Money is handled using decimal strings to avoid floating-point inaccuracies and guarantee deterministic hashing. Any financial transaction checks for proper constraints such as preventing zero-amount operations except where permitted (e.g., LOSS transactions).

## Transactions & Locks

Transactions are processed in PostgreSQL using strict serializability within a wallet boundary. 
- **Row Locks**: Before mutating balance, the system acquires a per-wallet row lock (`SELECT ... FOR UPDATE`) inside a `READ COMMITTED` transaction callback.
- **Atomic Operations**: `UpdateWallet`, `InsertTransaction`, `InsertLedger`, and `InsertOutbox` execute within the same database transaction.
- **Reconciliation**: Ledgers provide consistent paginated read-snapshots without blocking active transactions.

## Idempotency & Reversals

The system provides robust idempotency that works across both HTTP (`Idempotency-Key`) and asynchronous SQS payloads:
- A deterministic canonical JSON hash of business fields ensures identical payloads are safely replayed.
- Repeating an identical payload yields the persisted result. Different payloads with the same key are rejected with conflict errors.
- **Pending References**: When a referenced transaction (e.g., for a reversal) is missing, it creates a `PENDING_REFERENCE` with exponential backoff and a maximum TTL.
- **Reversals**: Reversals lookup previous transactions using external IDs tied strictly to their `providerId`. A transaction cannot be refunded twice.

## Inbox & Outbox Pattern
*(Provisional: SQS worker producers and consumers are pending implementation)*

The module avoids distributed transactions (2PC) by using the Transactional Outbox pattern:
- **Inbox**: SQS messages use `messageId` for deduplication. Inbox rows persist processing intent; messages are only deleted from the queue after durable commit. If processing fails transiently, messages are retried; if permanently failed, they move to the DLQ. SQS FIFO guarantees order and grouping.
- **Outbox**: Events (e.g., `WalletBalanceChanged`) are staged in the `outbox` table during the business transaction. An asynchronous background publisher claims batches using `SELECT ... SKIP LOCKED` to publish them to the outbound SQS events queue. This ensures zero data loss if the system crashes immediately after database commit but before message dispatch.

## Authentication (OIDC)
*(Provisional: The HTTP API and endpoint wiring are pending implementation)*

Business HTTP endpoints mandate external Identity Provider (IdP) authentication using OIDC. 
- Standard JWTs (`RS256` or `ES256`) are verified via an Fx-wired middleware (`*auth.Middleware`).
- The middleware rigorously validates `iss`, `aud`, expiration, and standard scope claims (`wagering:provider` or `wagering:internal`). 
- Isolation: Provider tokens must include a `provider_id` claim, restricting their operations and lookups to their data only.

## Limitations & Considerations

- **Database Sharding**: The current schema uses row-level locking on a single PostgreSQL instance and is not internally sharded. 
- **LocalStack IAM**: AWS IAM is not strictly enforced in local development (`ENFORCE_IAM=1` is disabled in the Compose setup), prioritizing fast queue provisioning.
- **JWT Introspection**: Revoked tokens remain technically valid until their `exp` time because validation relies purely on signature and standard claims rather than synchronous introspection or a revocation list.
