# Project status

Date: 2026-09-30 (America/Sao_Paulo)

## State

Implementation is active on local integration branch `carlosmeds/lead`. Orca Run `run_21967f35dd24` supervises the DAG. Remote push remains disabled. `SPEC.md` is immutable, ignored, and local; workers receive requirement IDs and handoffs instead of the full specification.

## Integrated tasks

- T01–T10 except T11, plus T12, T14 and provisional T18 documentation are committed locally. T02/T03/T05/T10 used Claude Sonnet; T12/T14/T18 used Antigravity; the Lead acted as Codex for T01/T04/T06/T07/T08/T09 after Orca Codex TUI readiness failures.
- R12 repaired Compose dependency healthchecks and added app/IdP provisioning. R13 repaired actual Keycloak access-token claims, issuer/JWKS configuration and queue policy targeting. R14 fixed PostgreSQL update timestamps exposed by live transaction tests.
- T11 (outbox worker) and T16 (real IdP/HTTP tests) are running in separate Claude Sonnet worktrees. The Lead is integrating and verifying the messaging path.

## Verification to date

- `go test ./...`, focused `go test -race`, and `go vet ./...` have passed after major integrations.
- Real PostgreSQL tests cover schema constraints, wallet opening, replay, inbox hash conflicts, pending references, competing reversals, duplicate concurrent BETs, and reference worker lease recovery.
- Compose builds and runs PostgreSQL, Keycloak, MiniStack and the app. Real Keycloak tokens were used to open a 100.00 BRL wallet, process and replay a BET, paginate its ledger, reconcile the final balance and verify provider isolation. A real inbound SQS message produced one BET/inbox commit and was deleted after commit. Malformed JSON reached the provisioned DLQ after redrive.

## Next work

Integrate T11 and T16 after handoff, diff review and relevant tests; dispatch T13/T15 when T11 unlocks them. Complete multi-process and crash tests T17, repair provisional documentation, then run independent adversarial review T19 against `SPEC.md` and remediate findings. No user approval gate applies to normal engineering decisions.
