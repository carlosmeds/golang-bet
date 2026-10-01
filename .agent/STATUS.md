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

No approval gates apply. Branch reported by the original worktree is `carlosmeds/lead`, HEAD `3f469e0`, remote `origin` is `https://github.com/carlosmeds/golang-bet.git`. The original shared Git common directory and root `.git` gitfile are read-only in this session. To preserve that Orca linkage, I did not replace it. A local, no-hardlink recovery Git copy is at `.agent/gitmeta`; it tracks the same `carlosmeds/lead` base and uses the current directory as its worktree via `GIT_DIR`/`GIT_WORK_TREE`. The exact original gitdir pointer is saved in `.agent/ORIGINAL_GITDIR.txt`. No remote update/push occurred. Recovery-copy commits are not reflected in the original common refs.

Recovery-copy commits:

- `b927954` outbox publisher and telemetry wiring
- `69d8344` readiness, health and SQS metrics
- `71e03a8` reversible migration CLI
- `6a4cb68` real IdP/HTTP authorization integration tests
- `9ef5d8e` independent-process recovery scenarios (implemented; live execution pending)
- `7801c7c` gofmt for the earlier T14 test files
- `93725c7` complete request correlation, SQS replay metrics and safe structured logs

Recovery-copy HEAD is being advanced locally and contains the implementation and recovery fixes. The original shared branch remains at `3f469e0`; Git writes are now available, and recovery commits will be transferred through a new integration worktree without altering the original worktree's pending changes.

Orca Run `run_21967f35dd24` is active. On 2026-10-01, the Orca runtime returned `app.running=true`, `runtime.state=ready`, and `graph.state=ready`. The coordinator received and verified the T11 (`95f1175`) and T16 (`9b041bf`) completion reports and acknowledged delivery `delivery_7b36d3465d32`. T17 and T19 remain pending in the DAG and will be dispatched through Orca.

`SPEC.md` remains immutable and local; SHA-256 `6298060871dd199a6ca5de2d6b7f007c4dccc887cc1c9ae1eb81982a4ca79135`.

## Task outcomes

- T01–T10, T12, T14 and T18 implementation is present; prior real Compose/Keycloak/SQS evidence and worker handoffs are retained.
- T11 outbox publisher, SQS sender adapter, stable IDs, lease/retry behavior, sequence ordering gate, metrics hooks and PostgreSQL integration tests are present. T11 handoff reports real PostgreSQL and race coverage passed.
- T13 health routes, DB/SQS readiness, structured JSON logging, aggregate Prometheus counters, reconciliation divergence reporting, and worker metrics hooks are present. Focused/unit/race tests pass; live readiness remains unverified now.
- T15 includes PostgreSQL/outbox/reference plus Keycloak/HTTP tests. T17 adds `tests/system/multiprocess_test.go`: it builds three independent service processes, sends required races, uses build-tag-only fault hooks for consumer commit-before-delete and outbox lease-crash windows, checks restart/reference/cross-transport recovery, and reconciles ledger sums. It creates isolated inbound/outbound FIFO queues per run to avoid interference from the Compose app consumer. `go test -race -count=1 ./tests/system` passed against live Compose PostgreSQL, Keycloak and MiniStack.
- T16 real Keycloak/HTTP authorization suites and handoff remain. Handoff reports full live integration and race tests passed before this sandbox's network restriction.
- T19 is not complete: launch an independent adversarial reviewer with the immutable `SPEC.md`, requirements, decisions and final integrated code; do not provide earlier review conclusions.

## Verification in the current environment

- Passed: focused `go test` for consumer, outbox, observability, and system-test package (system suite skips when integration environment variables are absent).
- Passed: compile-only `go test -run '^$' ./tests/system` and tagged `go test -tags=systemfault -run '^$' ./tests/system`.
- Passed: `GOCACHE=/tmp/gocache go vet ./...` after formatting and observability fixes.
- Passed: focused tests for observability, consumer, outbox, reference, migration and the default/tagged T17 builds.
- Passed: focused `go test -race` for observability, consumer, outbox and reference worker before the final log-only changes; rerun after final code changes.
- Attempted: `go test ./...` and `go test -race ./...`; both fail at `internal/auth` local `httptest` listener creation due socket policy. Other package results in each run passed.
- Passed: `docker compose config` and `docker compose up -d --build --wait`; all five services reported healthy, including the application.
- Passed: live T17 with `-race`, three independent app processes, temporary PostgreSQL database and per-run FIFO queues; all five scenarios passed.
- Prior environment restriction: Docker/TCP access was denied. The current environment has full access; Compose bootstrap and all live tests are being retried.
- A full `go test ./...` earlier failed only when `internal/auth`'s `httptest.NewServer` attempted to listen on a local TCP socket; packages in that run otherwise passed.

## Remaining work

1. Transfer recovery-copy commits into a new integration worktree while preserving the original worktree's uncommitted state.
2. Run the independent T19 adversarial review; turn each valid finding into a repair task and verify its fix.
3. Re-run gofmt, vet, all tests, Compose and system scenarios after repairs.
4. Commit remaining changes logically and prepare the final local integration branch. Keep remote push and main merge disabled until verification is complete.

The project remains active and incomplete; no completion claim is made while T17 live execution or T19 review is outstanding.
