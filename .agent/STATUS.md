# Project status

Date: 2026-10-01 (America/Sao_Paulo)

## State

```yaml
phase: implementation
status: active
create_workers_now: true
implement_code_now: true
push_remote_now: false
```

The original linked worktree remains on `carlosmeds/lead` at `3f469e0` with all prior uncommitted files preserved. Recovery commits and the independent T19 review commit are integrated on the separate local branch `carlosmeds/lead-integration`; no push or main merge occurred.

Orca Run `run_21967f35dd24` is active and the runtime is ready. T11, T16, T17 and R18 completion reports were inspected. T19 completed as an independent Claude Opus review at high effort in a separate worktree. The report found 2 high, 5 medium and several low findings; repair tasks are now active. No remote push or main merge occurred.

`SPEC.md` is local, ignored and unchanged. SHA-256 in the source checkout and reviewer checkout: `6298060871dd199a6ca5de2d6b7f007c4dccc887cc1c9ae1eb81982a4ca79135`.

## Task outcomes

- T01–T14, T16–T18 and T17 implementation are integrated on `carlosmeds/lead-integration`; T11/T13/T15 and T16 now have current verification evidence.
- T11 outbox publisher uses leased claims, stable event identity, per-aggregate ordering, retries, metrics and graceful shutdown. Live PostgreSQL crash, contention and ordering tests pass under `-race`.
- T13 health/readiness, structured identifiers in logs and metrics are implemented; live Compose readiness passed.
- T15 PostgreSQL, reference, outbox, Keycloak/HTTP and SQS multi-process integration suites passed against Compose under `-race`.
- T16 IdP/HTTP authorization tests now compose the current metrics and SQS modules through repair R18 (`d0eb026`).
- T17 starts three independent service processes, creates isolated FIFO queues, exercises replay/race/restart/crash/recovery and reconciles the ledger. It passed independently four times and in the sequential live integration batch.
- T19 read the immutable `SPEC.md`, full `.agent/REQUIREMENTS.md`, full `.agent/DECISIONS.md` and final integrated code without previous review conclusions. Its per-requirement matrix and findings are in `.agent/handoffs/T19.md`.
- T19's redrive tests, README correction and ARCHITECTURE correction are integrated in commit `0c770a2`; `WAGERING_TEST_SQS_ENDPOINT=http://localhost:4566 go test -race -count=1 -v ./tests/integration/sqs` passed.
- R19-F4 (transient PostgreSQL errors return HTTP 503 with `Retry-After`) is integrated in `549676b`; focused tests and vet pass. The worker's HTTP integration cases skipped without live endpoint variables, so the root Compose verification must rerun them.
- R19-F1 (broker security/API audience) and R19-F3 (outbox throughput) are active; R19-F10C (safe request log IDs and tidy) is active on the completed F4 Codex terminal. R19-F2 depends on R19-F1; metrics, concurrency coverage, shutdown timeout, inbox replay, reference retry and documentation repairs are tracked in Orca.

## Verification

- Passed: `gofmt` over all Go files, `go vet ./...`, `go test ./...`, and `go test -race ./...`.
- Passed: `docker compose config`, `docker compose up -d --build --wait`; PostgreSQL, Keycloak, MiniStack and the application reported healthy. `GET /health/ready` returned `ready`.
- Passed: live system suite under `-race`, including 50 duplicate requests through three processes, two competing 80 BETs against balance 100, independent wallets, HTTP/SQS replay crossing, inbox commit-before-delete crash/redelivery, pending-reference resolution after restart, outbox lease recovery in another process, and final ledger reconciliation.
- Passed: real live integration batch with `-race -p 1`: `./tests/...`, `./internal/storage/pg`, `./internal/workers/reference`, and `./internal/workers/outbox`.
- Passed: R18 real Keycloak/PostgreSQL auth and HTTP tests under `-race`, including SQS-backed readiness and metrics.
- Passed: real migration CLI smoke on a disposable PostgreSQL database: up, down one migration, up, then drop the scratch database.
- One live integration batch using default package parallelism timed out before the T17 consumer crash marker while auth/HTTP and database packages passed. T17 independently passed four times, and the full live batch passed with `-p 1`; README documents that setting for shared Compose services.
- `git diff --check` passed before the latest status/decision documentation update; rerun it before final commit.

## Remaining work

1. Integrate and verify all valid T19 repairs, including the missing system concurrency cases.
2. Complete the documentation and requirements evidence updates after the implementation changes settle.
3. Rerun gofmt, both vet configurations, unit/race tests, the live Compose integration suites, T17 multi-process recovery and Compose startup/readiness.
4. Commit the final local integration branch cleanly. Do not push or merge into main.

The project remains active until T19 has no unresolved critical/high findings and all post-review verification passes.
