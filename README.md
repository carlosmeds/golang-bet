# Wagering System

A multi-provider wagering ledger with durable idempotency across HTTP and SQS. Financial amounts use integer minor units in PostgreSQL and decimal strings at the API boundary. PostgreSQL is the authority for balances, ledger entries, inbox receipts, retries, and pending outbox events.

## Local bootstrap

Requirements: Go 1.23.0 and Docker Compose v2.

```sh
docker compose up -d --build --wait
```

Compose provisions PostgreSQL, Keycloak, MiniStack SQS queues/policies, and starts the application. The service applies versioned SQL migrations at startup. Compose supplies its own container URLs; `.env.example` documents host-side local values and is not needed to start the Compose stack.

The default local endpoints are API `http://localhost:8081`, Keycloak `http://localhost:8082`, PostgreSQL `localhost:54320`, and the MiniStack SQS gateway `http://localhost:4566`.

### Configuration

The application reads these variables (Compose sets them for the container; `.env.example` has host-side values):

| Variable | Default | Purpose |
| --- | --- | --- |
| `DATABASE_URL`, `HTTP_ADDR`, `AWS_REGION`, `WAGER_QUEUE_URL`, `EVENT_QUEUE_URL`, `OIDC_ISSUER_URL`, `OIDC_AUDIENCE` | required (`HTTP_ADDR` `:8080`, `AWS_REGION` `us-east-1`) | database, listener, region, inbound and outbound queues, token issuer and audience (`wagering-api`) |
| `SQS_ENDPOINT`, `AWS_PROFILE`, `AWS_SHARED_CREDENTIALS_FILE` | unset | broker endpoint and AWS SDK credentials profile (`wagering-app`) |
| `OIDC_JWKS_URL`, `OIDC_PROVIDER_CLAIM` | discovery, `provider_id` | optional JWKS override and provider claim name |
| `SHUTDOWN_TIMEOUT` | `30s` | bound for the whole graceful shutdown |
| `REFERENCE_RETRY_BASE_DELAY`, `_MAX_DELAY`, `_TTL`, `_MAX_ATTEMPTS` | `1s`, `5m`, `24h`, `0` (no cap) | pending-reference backoff and expiry |
| `REFERENCE_WORKER_BATCH_SIZE`, `_CONCURRENCY`, `_LEASE`, `_ITEM_TIMEOUT`, `_POLL_INTERVAL` | `20`, `4`, `1m`, `20s`, `1s` | reference worker tuning |
| `OUTBOX_BACKOFF_BASE_DELAY`, `_BACKOFF_MAX_DELAY`, `OUTBOX_BATCH_SIZE`, `_CONCURRENCY`, `_LEASE`, `_SEND_TIMEOUT`, `_ACK_TIMEOUT`, `_POLL_INTERVAL` | `1s`, `5m`, `20`, `4`, `30s`, `10s`, `5s`, `500ms` | outbox publisher tuning |

The SQS consumer settings (long poll 10 s, 10 messages, visibility 60 s, backoff cap 60 s) are compiled defaults in `internal/messaging/consumer`, not environment variables.

### Broker access (REQ-062, D12)

MiniStack itself is not published: it treats the `test` key and unsigned requests as root, so all access goes through `sqs-gateway` (nginx), which forwards only requests signed by a provisioned IAM user and blocks the emulator's admin endpoints. `infra/localstack/init-sqs.sh` creates the identities and policies and fails closed (the broker stays unhealthy, so the application does not start, if any step fails): `wagering-producer` may only `SendMessage` to `wager-transactions.fifo` (a queue policy also denies every other principal), `wagering-app` consumes the inbound queue and publishes `wager-events.fifo`, `wagering-broker-operator` manages queues for tests and operations, and `wagering-provider-probe` has no queue access. The application reads its credentials from the `broker-credentials` volume (`AWS_PROFILE=wagering-app`). MiniStack does not verify SigV4 signatures, so an access key id is the effective credential locally; real SQS verifies signatures.

To run host-side tests against the broker, copy the generated identities out (keys change whenever the broker container restarts):

```sh
infra/localstack/export-broker-credentials.sh
export WAGERING_TEST_BROKER_CREDENTIALS_FILE="$PWD/tmp/broker-credentials"
```

Operational endpoints are `GET /health/live`, `GET /health/ready` (PostgreSQL and inbound SQS checks), and `GET /metrics` (Prometheus text format).

## Migrations

The application applies pending migrations during startup. To manage them manually, set a host-reachable database URL:

```sh
DATABASE_URL='postgres://wagering:wagering_password@localhost:54320/wagering?sslmode=disable' go run ./cmd/migrate -direction up
DATABASE_URL='postgres://wagering:wagering_password@localhost:54320/wagering?sslmode=disable' go run ./cmd/migrate -direction down -count 1
```

`-count 0` rolls back all applied migrations. Migrations are plain, versioned SQL under `migrations/`; each has an up and down direction.

## Authentication and API

Keycloak provisions `provider-a`, `provider-b`, and `internal-service` confidential clients with client credentials. Local test secrets are in `infra/keycloak/realm-export.json`; use secrets from a real secret store outside local development.

```sh
TOKEN=$(curl -fsS -X POST http://localhost:8082/realms/wagering/protocol/openid-connect/token \
  -d 'client_id=provider-a' -d 'client_secret=provider-a-secret' \
  -d 'grant_type=client_credentials' | jq -r .access_token)
INTERNAL_TOKEN=$(curl -fsS -X POST http://localhost:8082/realms/wagering/protocol/openid-connect/token \
  -d 'client_id=internal-service' -d 'client_secret=internal-secret' \
  -d 'grant_type=client_credentials' | jq -r .access_token)
```

Tokens carry the `wagering-api` audience (`OIDC_AUDIENCE`). Provider tokens add the claims `provider_id` and `roles: wagering:provider`; the internal-service token carries `roles: wagering:internal`.

Use the internal-service token for wallet creation and wallet/ledger/reconciliation routes. Provider tokens can submit wagering operations and read only their own transactions. Routes are `POST /wallets`, `GET /wallets/{walletId}`, `GET /wallets/{walletId}/ledger`, `POST /wallets/{walletId}/reconciliation`, `POST /wagering/transactions`, `GET /wagering/transactions/{transactionId}`, and `GET /providers/{providerId}/wagering/transactions/{externalTransactionId}`. Wager submission requires an `Idempotency-Key` header; money is JSON `{ "amount": "12.34", "currency": "BRL" }` (one or two decimals accepted, always returned with two; see [Money](ARCHITECTURE.md#money)).

Example wallet opening and wager:

```sh
WALLET_ID=$(curl -fsS -X POST http://localhost:8081/wallets \
  -H "Authorization: Bearer $INTERNAL_TOKEN" -H 'Content-Type: application/json' \
  -d '{"playerId":"player-123","initialBalance":{"amount":"100.00","currency":"BRL"}}' | jq -r .id)
curl -sS -X POST http://localhost:8081/wagering/transactions \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -H 'Idempotency-Key: bet-1' \
  -d '{"providerId":"provider-a","externalTransactionId":"bet-1","playerId":"player-123","walletId":"'"$WALLET_ID"'","roundId":"round-1","gameId":"game-1","kind":"BET","money":{"amount":"25.00","currency":"BRL"}}'
```

Repeating the same request returns `200` with `idempotentReplay: true`; the first answer is `201`.

### Responses

| Status | Meaning |
| --- | --- |
| `201` | wallet created, or new operation `PROCESSED` |
| `202` | new operation `PENDING_REFERENCE` (a referenced transaction has not arrived or finalized yet; it resolves automatically) |
| `422` | new operation `REJECTED`; the body is the transaction with a stable `failureCode` |
| `200` | idempotent replay, or a read |
| `400` `INVALID_REQUEST` | invalid input; nothing was stored, fix it and resend |
| `401` / `403` / `404` | `UNAUTHENTICATED` / `FORBIDDEN` / `NOT_FOUND` (another provider's data is also `404`) |
| `409` | `HASH_MISMATCH` (same key, different content) or `CONFLICT` (external ID reused with another key; wallet exists) |
| `503` `TEMPORARILY_UNAVAILABLE` | transient failure, nothing finalized; retry the same request after `Retry-After: 5` |

Errors use `{"code","message","correlationId"}`. The catalog of `failureCode` values, which results are definitive, hash fields, state machine, reversal rules and reference expiry are in [ARCHITECTURE.md](ARCHITECTURE.md).

## Queue behavior

MiniStack provisions `wager-transactions.fifo`, `wager-transactions-dlq.fifo`, and `wager-events.fifo`. The consumer records SQS `messageId` in its PostgreSQL inbox in the same transaction as financial effects. It deletes a receipt only after commit; invalid messages are left for queue redrive, and transient failures use bounded visibility backoff (see "Retry and redrive bounds" below). On shutdown, the message in flight is cancelled (its transaction rolls back) and every unfinished receipt of the batch is made visible again. The default inbound visibility is 60 seconds; long-poll is 10 seconds and requests contain at most 10 messages.

### Retry and redrive bounds

- **Visibility:** a received message is hidden for 60 s. A transient processing failure resets it to 1 s, 2 s, 4 s, 8 s, 16 s, 32 s, then 60 s for every later receive (`MaxBackoffSeconds`). A poison (malformed or permanently invalid) message is retried every 1 s.
- **Redrive:** `wager-transactions.fifo` moves a message to `wager-transactions-dlq.fifo` after 15 receives (`maxReceiveCount` in `infra/localstack/init-sqs.sh`). A poison message therefore reaches the DLQ in about 15 s. A message that keeps failing transiently while PostgreSQL still answers its readiness ping is dead-lettered after about 10 minutes of backoff (1+2+4+8+16+32+9x60 s).
- **PostgreSQL outage:** before each receive the consumer pings PostgreSQL; while the ping fails it receives nothing (logging one `SQS receives paused` warning, `receives resumed` afterwards), so an outage of any length spends none of the redrive budget and valid wagers stay on the queue until the database returns. If the database fails mid-batch, the remaining unstarted messages are released unprocessed. Messages already in flight at the moment of failure cost one receive each.
- **Residual window:** if the database is reachable but every commit keeps failing for more than the ~10 minute backoff window, the wager is dead-lettered and needs a manual redrive from the DLQ (SQS console/CLI `start-message-move-task`, or re-sending the body).
- **Shutdown (SIGTERM):** the in-flight message is cancelled and unstarted receipts are made visible again (visibility 0); receipts are never deleted on shutdown. If cancellation lands after commit but before delete, the redelivery is answered from the inbox.
- **Invalid messages:** unparseable bodies, unsupported `type`, a `messageId` reused with another body, a key reused with other content and other domain-invalid input are never deleted by the consumer; they are re-shown every second and redriven to the DLQ. Business rejections are not invalid: they are stored results and the message is deleted.

FIFO ownership: the internal producer that sends to `wager-transactions.fifo` sets `MessageGroupId` (the wallet ID) and `MessageDeduplicationId` (the `messageId`); the application only consumes that queue and does not depend on either value. The application sets `MessageGroupId` to the wallet ID (aggregate) and `MessageDeduplicationId` to the stable `eventId` on every message it sends to `wager-events.fifo`. Message and event schemas, the failure/transient policy and routing are specified in [ARCHITECTURE.md](ARCHITECTURE.md#inbound-sqs).

### Outbound events

Events are read from the transactional outbox and sent to `wager-events.fifo` with message attributes `eventId` and `eventType`; the body is the JSON envelope (`WagerTransactionProcessed`, `WagerTransactionRejected`, `WagerTransactionPendingReference`, `WalletBalanceChanged`). Per-wallet order is preserved. Consumers must deduplicate by `eventId` because a crash after broker acceptance but before recording publication delivers the same event again. Only `wagering-app` may send to the events queue; to inspect it locally, export the broker credentials and use the `wagering-broker-operator` profile:

```sh
AWS_SHARED_CREDENTIALS_FILE="$PWD/tmp/broker-credentials" AWS_PROFILE=wagering-broker-operator AWS_REGION=us-east-1 \
  aws --endpoint-url http://localhost:4566 sqs receive-message --max-number-of-messages 10 \
  --message-attribute-names All --queue-url http://localhost:4566/000000000000/wager-events.fifo
```

Reference retries and outbox publication use database leases, bounded backoff, and recover after process restart. Pending references expire after 24 hours (see ARCHITECTURE.md). Known limitations, including that a permanently unpublishable outbox event blocks later events of its wallet until the cause is fixed, are listed in [ARCHITECTURE.md](ARCHITECTURE.md#decisions-and-limitations).

## Verification

```sh
go test ./...
go test -race ./...
go vet ./...
```

The default suite includes unit tests and skips live-dependency tests when their environment variables are unset. With Compose running, run database/outbox integration tests using an administrative URL that can create scratch databases:

```sh
WAGERING_TEST_ADMIN_URL='postgres://wagering:wagering_password@localhost:54320/postgres?sslmode=disable' go test ./internal/storage/pg ./internal/workers/reference ./internal/workers/outbox -count=1
WAGERING_TEST_ADMIN_URL='postgres://wagering:wagering_password@localhost:54320/postgres?sslmode=disable' \
WAGERING_TEST_KEYCLOAK_URL='http://localhost:8082' WAGERING_TEST_SQS_ENDPOINT='http://localhost:4566' \
WAGERING_TEST_BROKER_CREDENTIALS_FILE="$PWD/tmp/broker-credentials" WAGERING_TEST_COMPOSE_APP_URL='http://localhost:8081' \
go test -p 1 ./tests/... -count=1
```

For repeatable multi-instance and crash-window verification, see [ARCHITECTURE.md](ARCHITECTURE.md) for implemented guarantees and current test coverage. The `tests/integration` packages use real Keycloak and PostgreSQL; messaging tests use the real AWS SQS-compatible endpoint supplied through the local stack.

The PostgreSQL-outage scenario in `tests/integration/sqs` is skipped unless the Compose stack is addressed. It stops the Compose PostgreSQL container for about 20 seconds, so run it only against a disposable stack:

```sh
WAGERING_TEST_COMPOSE_POSTGRES_CONTAINER="$(docker compose ps -q postgres)" WAGERING_TEST_COMPOSE_APP_URL='http://localhost:8081' \
WAGERING_TEST_ADMIN_URL='postgres://wagering:wagering_password@localhost:54320/postgres?sslmode=disable' \
WAGERING_TEST_KEYCLOAK_URL='http://localhost:8082' WAGERING_TEST_SQS_ENDPOINT='http://localhost:4566' \
WAGERING_TEST_BROKER_CREDENTIALS_FILE="$PWD/tmp/broker-credentials" \
go test -count=1 -run TestComposeValidWagerSurvivesPostgresOutage ./tests/integration/sqs
```

The independent-process recovery suite launches three `cmd/wagering` OS processes and a second binary compiled with test-only crash hooks:

```sh
WAGERING_TEST_ADMIN_URL='postgres://wagering:wagering_password@localhost:54320/postgres?sslmode=disable' \
WAGERING_TEST_KEYCLOAK_URL='http://localhost:8082' \
WAGERING_TEST_SQS_ENDPOINT='http://localhost:4566' \
WAGERING_TEST_BROKER_CREDENTIALS_FILE="$PWD/tmp/broker-credentials" \
go test -race -count=1 ./tests/system
```

It creates isolated FIFO queues for each run, then exercises 50 duplicate requests, the 80/80 race, independent wallets, HTTP/SQS replay, process restart, commit-before-delete redelivery, pending-reference recovery, outbox lease recovery in a second process, and ledger reconciliation. `systemfault` hooks are excluded from normal builds and used only for this test binary.
