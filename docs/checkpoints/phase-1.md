# Phase 1 — Foundations: checkpoint

Status: IN PROGRESS

## Plan (written before coding)

Scope per brief section 11: full schema and migrations, DB roles and grants, ledger with posting rules and
balance trigger, identity (OTP, sessions, refresh rotation, SSO linking), KYC with mock adapter, RG limits,
wallet core, idempotency middleware, rate limiting, audit log, clock abstraction, OTel wiring.

Acceptance: ledger property test, idempotency tests, negative-balance concurrency test, RG tests and
auth/IDOR tests pass; ERD generated into `docs/erd.svg`.

### Slice 1 — schema and database invariants
- Migrations (goose, forward-compatible): `00002_platform` (schemas, roles, uuid_v7, mutation guard),
  `00003_identity_kyc_rg`, `00004_ledger_wallet`, `00005_payments_payouts`, `00006_game_tickets_draws`,
  `00007_audit_idempotency`, `00008_grants` (all privileges in one auditable file).
- `internal/platform/clock`: `Clock` interface, fixed/stepped test clocks, Postgres-transaction clock.
- `internal/platform/testpg`: template-database cloning so each integration test gets a migrated DB quickly.
- Tests (integration, real Postgres): unbalanced journal rejected at commit; ledger/wallet_transactions/audit_log
  append-only for app roles and for table owners; negative balance rejected; wallet balance must equal the sum
  of its transactions at commit; fifth ticket / duplicate seq rejected; draw seed immutable once set; kra_outbox
  worker may only set exported_at; app roles cannot DDL; only app_migrator owns tables.

### Slice 2 — ledger + wallet
Pure posting functions (table-driven), journal writer, wallet credit/debit with atomic conditional UPDATE,
rapid property test over random sequences, 50-goroutine negative-balance test.

### Slice 3 — idempotency, rate limiting, audit
`Idempotency-Key` middleware (request hash, stored response, 422 on mismatch), Valkey sliding-window limiter that
fails closed on money/play routes, audit writer.

### Slice 4 — identity, KYC, RG
OTP (HMAC, expiry, attempts, cooldown), JWT EdDSA access tokens, hashed refresh tokens with rotation + reuse
detection, SSO linking rules, KYC adapter interface + mock, RG limits/time-out/self-exclusion with cooling-off,
auth bypass and IDOR tests on every endpoint.

### Slice 5 — OTel, ERD, checkpoint report.

## Results
(filled in at the end of the phase)
