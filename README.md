# Wagering System

A multi-provider wagering ledger with durable idempotency across HTTP and SQS. Financial amounts use integer minor units in PostgreSQL and decimal strings at the API boundary. PostgreSQL is the authority for balances, ledger entries, inbox receipts, retries, and pending outbox events.

## Local bootstrap

Requirements: Go 1.23.0 and Docker Compose v2.

```sh
docker compose up -d --build --wait
```

Compose provisions PostgreSQL, Keycloak, MiniStack SQS queues/policies, and starts the application. The service applies versioned SQL migrations at startup. Compose supplies its own container URLs; `.env.example` documents host-side local values and is not needed to start the Compose stack.

The default local endpoints are API `http://localhost:8081`, Keycloak `http://localhost:8082`, PostgreSQL `localhost:54320`, and MiniStack `http://localhost:4566`.

Operational endpoints are `GET /health/live`, `GET /health/ready` (PostgreSQL and inbound SQS checks), and `GET /metrics` (Prometheus text format).

## Migrations

The application applies pending migrations during startup. To manage them manually, set a host-reachable database URL:

```sh
DATABASE_URL='postgres://wagering:wagering_password@localhost:54320/wagering?sslmode=disable' go run ./cmd/migrate -direction up
DATABASE_URL='postgres://wagering:wagering_password@localhost:54320/wagering?sslmode=disable' go run ./cmd/migrate -direction down -count 1
```

`-count 0` rolls back all applied migrations. Migrations are plain, versioned SQL under `migrations/`; each has an up and down direction.

## Authentication and API

Keycloak provisions `provider-a`, `provider-b`, and `internal-service` confidential clients with client credentials. Local test secrets are in `infra/keycloak/realm.json`; use secrets from a real secret store outside local development.

```sh
TOKEN=$(curl -fsS -X POST http://localhost:8082/realms/wagering/protocol/openid-connect/token \
  -d 'client_id=provider-a' -d 'client_secret=provider-a-secret' \
  -d 'grant_type=client_credentials' | jq -r .access_token)
INTERNAL_TOKEN=$(curl -fsS -X POST http://localhost:8082/realms/wagering/protocol/openid-connect/token \
  -d 'client_id=internal-service' -d 'client_secret=internal-secret' \
  -d 'grant_type=client_credentials' | jq -r .access_token)
```

Use the internal-service token for wallet creation and wallet/ledger/reconciliation routes. Provider tokens can submit wagering operations and read only their own transactions. Routes are `POST /wallets`, `GET /wallets/{walletId}`, `GET /wallets/{walletId}/ledger`, `POST /wallets/{walletId}/reconciliation`, `POST /wagering/transactions`, `GET /wagering/transactions/{transactionId}`, and `GET /providers/{providerId}/wagering/transactions/{externalTransactionId}`. Wager submission requires `Idempotency-Key`; money is JSON `{ "amount": "12.34", "currency": "BRL" }`.

Example wallet opening:

```sh
curl -fsS -X POST http://localhost:8081/wallets \
  -H "Authorization: Bearer $INTERNAL_TOKEN" -H 'Content-Type: application/json' \
  -d '{"playerId":"player-123","initialBalance":{"amount":"100.00","currency":"BRL"}}'
```

## Queue behavior

MiniStack provisions `wager-transactions.fifo`, `wager-transactions-dlq.fifo`, and `wager-events.fifo`. The consumer records SQS `messageId` in its PostgreSQL inbox in the same transaction as financial effects. It deletes a receipt only after commit; invalid messages are left for queue redrive, and transient failures use bounded visibility backoff. On shutdown, unstarted batch receipts are made visible again. The default inbound visibility is 60 seconds; long-poll is 10 seconds and requests contain at most 10 messages.

FIFO grouping and deduplication use the wallet aggregate identifier and stable event ID respectively. Outbound events are read from the transactional outbox and sent to `wager-events.fifo`; consumers should deduplicate by `eventId` because a crash after broker acceptance but before recording publication can cause the same event to be delivered again. Reference retries and outbox publication use database leases, bounded retries, and recover after process restart.

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
go test -p 1 ./tests/... -count=1
```

For repeatable multi-instance and crash-window verification, see [ARCHITECTURE.md](ARCHITECTURE.md) for implemented guarantees and current test coverage. The `tests/integration` packages use real Keycloak and PostgreSQL; messaging tests use the real AWS SQS-compatible endpoint supplied through the local stack.

The independent-process recovery suite launches three `cmd/wagering` OS processes and a second binary compiled with test-only crash hooks:

```sh
WAGERING_TEST_ADMIN_URL='postgres://wagering:wagering_password@localhost:54320/postgres?sslmode=disable' \
WAGERING_TEST_KEYCLOAK_URL='http://localhost:8082' \
WAGERING_TEST_SQS_ENDPOINT='http://localhost:4566' \
go test -race -count=1 ./tests/system
```

It creates isolated FIFO queues for each run, then exercises 50 duplicate requests, the 80/80 race, independent wallets, HTTP/SQS replay, process restart, commit-before-delete redelivery, pending-reference recovery, outbox lease recovery in a second process, and ledger reconciliation. `systemfault` hooks are excluded from normal builds and used only for this test binary.
