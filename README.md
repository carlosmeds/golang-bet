# Wagering System

A robust, idempotent, and highly available multi-tenant wagering ledger system.

## Setup & Bootstrap

This project requires Go 1.23.0 and Docker.

1. **Environment setup**: Copy the example environment variables.
   ```sh
   cp .env.example .env
   ```

2. **Infrastructure**: Start PostgreSQL, LocalStack (SQS), and Keycloak locally.
   ```sh
   docker compose up -d --wait
   ```

## Queues (SQS)
*(Provisional: SQS worker consumers and producers are pending implementation in upcoming tasks)*

The system relies on AWS SQS FIFO queues. When using Docker Compose, LocalStack automatically provisions these queues via `infra/localstack/init-sqs.sh`:
- `wager-transactions.fifo`: Inbound queue for asynchronous transaction processing.
- `wager-transactions-dlq.fifo`: Dead Letter Queue for poison-pill transactions that exhaust retry limits.
- `wager-events.fifo`: Outbound queue for system events (wallet balance changes, processed transactions).

## Migrations
*(Provisional: The dedicated migration command/runner is pending implementation)*

Database migrations are defined in the `migrations/` directory using plain SQL and the `golang-migrate` naming convention. Once the runner is implemented, they can be applied. Alternatively, you can run them manually using the `golang-migrate` CLI:

**Up (Apply):**
```sh
migrate -path migrations -database "postgres://wagering:wagering_password@localhost:54320/wagering?sslmode=disable" up
```

**Down (Rollback):**
```sh
migrate -path migrations -database "postgres://wagering:wagering_password@localhost:54320/wagering?sslmode=disable" down
```

## Authentication
*(Provisional: The HTTP API and route protection are pending implementation in upcoming tasks)*

Business endpoints require external OIDC authentication. The local Keycloak container automatically provisions a `wagering` realm and a `wagering-client` to support client credentials flow.

Example of obtaining a token and calling the service:
```sh
# Fetch a JWT from Keycloak
TOKEN=$(curl -sX POST http://localhost:8082/realms/wagering/protocol/openid-connect/token \
  -d "client_id=wagering-client" \
  -d "grant_type=client_credentials" \
  | jq -r .access_token)

# Use the token for API requests (once endpoints are implemented)
curl -H "Authorization: Bearer $TOKEN" http://localhost:8081/v1/wallets/...
```

## Testing

The test suite covers unit, integration, concurrency, and fault tolerance scenarios.

**Unit & Linter Tests:**
```sh
go test ./...
go vet ./...
go test -race ./...
```

**Integration Tests:**
*(Provisional: Full integration test suites, multi-process tests, and fault tests are pending)*
Require local infrastructure running (`docker compose up -d`). Pass the administrative database URL to run integration suites:
```sh
WAGERING_TEST_ADMIN_URL="postgres://wagering:wagering_password@localhost:54320/wagering?sslmode=disable" go test ./... -v -count=1
```

**Multi-Instance & Fault Tests:**
Tests simulating multi-process concurrent updates and fault-injection (crash and recovery, outbox lag, duplicate deliveries):
```sh
# Multi-instance race tests:
WAGERING_TEST_ADMIN_URL="postgres://wagering:wagering_password@localhost:54320/wagering?sslmode=disable" go test -run TestMultiProcess ./...

# Fault-injection tests:
WAGERING_TEST_ADMIN_URL="postgres://wagering:wagering_password@localhost:54320/wagering?sslmode=disable" go test -run TestFaultInjection ./...
```
