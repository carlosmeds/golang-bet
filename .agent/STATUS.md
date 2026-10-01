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

The original linked worktree remains on `carlosmeds/lead` at `3f469e0` with all prior uncommitted files preserved. The recovery commits have been fetched into the local Git common repository and integrated on the separate local branch `carlosmeds/lead-integration`; its current code/test commits are `4e78f23`, `44f2021`, `db3e9e7`, and `d0eb026`, after the recovery history. The `carlosmeds/lead-recovery` branch preserves the first transfer point. The original worktree was not reset or overwritten. No push or main merge occurred.

Orca Run `run_21967f35dd24` is active and the runtime is ready. T11 and T16 completion reports were inspected and acknowledged. T17 and repair R18 are completed in the DAG; the only remaining gate is T19 independent adversarial review and any repairs it generates. Claude OAuth is expired in Orca; Codex authentication is active and the configured model is `gpt-6-sol`. T19 is allocated to a separate Codex session at xhigh reasoning.

`SPEC.md` is local, ignored and unchanged. SHA-256 in the source checkout and reviewer checkout: `6298060871dd199a6ca5de2d6b7f007c4dccc887cc1c9ae1eb81982a4ca79135`.

## Task outcomes

- T01–T14, T16–T18 and T17 implementation are integrated on `carlosmeds/lead-integration`; T11/T13/T15 and T16 now have current verification evidence.
- T11 outbox publisher uses leased claims, stable event identity, per-aggregate ordering, retries, metrics and graceful shutdown. Live PostgreSQL crash, contention and ordering tests pass under `-race`.
- T13 health/readiness, structured identifiers in logs and metrics are implemented; live Compose readiness passed.
- T15 PostgreSQL, reference, outbox, Keycloak/HTTP and SQS multi-process integration suites passed against Compose under `-race`.
- T16 IdP/HTTP authorization tests now compose the current metrics and SQS modules through repair R18 (`d0eb026`).
- T17 starts three independent service processes, creates isolated FIFO queues, exercises replay/race/restart/crash/recovery and reconciles the ledger. It passed independently four times and in the sequential live integration batch.
- T19 has not run. It must receive the immutable `SPEC.md`, `REQUIREMENTS.md`, `DECISIONS.md` and final code, with no previous review conclusions. Valid findings must become repair tasks.

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

1. Commit the current verification and model-allocation documentation changes.
2. Create a separate Orca-managed T19 review worktree from `carlosmeds/lead-integration`; copy `SPEC.md` locally and confirm its hash before dispatch.
3. Review every mandatory requirement and actively search for monetary, transaction, concurrency, auth, idempotency, replay, inbox/outbox, pending-reference, recovery and shutdown failures.
4. Create and verify repair tasks for every valid finding, then rerun formatting, vet, all tests, live integrations, Compose and multi-process recovery.
5. Commit the final local integration branch cleanly. Do not push or merge into main.

The project remains active until T19 has no unresolved critical/high findings and all post-review verification passes.
