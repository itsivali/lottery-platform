# Phase 1 — Foundations: checkpoint

Status: COMPLETE — awaiting human go-ahead

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

All work landed through squash-merged PRs with a green `ci-ok`: #12 schema, #13 ledger and wallet, #14 idempotency,
rate limit and audit, #15 identity, #16 KYC and responsible gaming, #17 HTTP API and OpenAPI, plus the PR for this
report (telemetry, ERD, checkpoint). Final local run of what CI runs (`make ci`):

- lint: `0 issues` (golangci-lint, go vet, actionlint, gofmt)
- 169 top-level tests across unit, integration (real Postgres 16 and Valkey 8 via testcontainers) and property tests,
  all passing with `-race -shuffle=on -count=1`
- coverage 88.1% overall (gate 70%); `internal/wallet` 87.5% and `internal/ledger` 94.5% (gate 85%). Coverage is now
  measured with `-coverpkg=./...` and duplicate profile blocks are merged in `scripts/check-coverage.sh`; sqlc-generated
  `store` packages are excluded like `main.go` files.
- `make e2e` (compose stack with the telemetry-wired api binary): `e2e smoke ok`
- `make erd` regenerates `docs/erd.svg` from a freshly migrated database: 32 tables, 32 foreign keys.
- `npx @redocly/cli lint api/openapi.yaml`: valid (4 tag-description warnings).

### Acceptance (brief section 11, phase 1)

| Criterion | Evidence |
|---|---|
| Ledger property test | `TestLedgerWallet_Property_RandomSequencesKeepEverythingBalanced`: rapid, 100 random sequences of top-ups, plays, refunds, withdrawals, failed withdrawals, deposit reversals and prize payouts over two players; after every step all journals balance, every wallet equals the sum of its transactions and the ledger's player liability, and the balance is never negative. Pure rule properties in `internal/ledger`. |
| Idempotency tests | `internal/platform/idempotency` (13 tests): replay, 422 on changed body or path, 409 for a concurrent duplicate (20 racing requests run the handler once), per-principal scope, 5xx not cached, expiry reuse, stale takeover. Plus `TestAPI_ReplayedRequestsReturnTheStoredResponseWithoutRepeatingTheEffect`. |
| Negative-balance concurrency | `TestWallet_ConcurrentStakesNeverOverdraw` (50 goroutines on KES 1,000: exactly 10 succeed) and `TestWallet_ConcurrentStakesAndWithdrawalsNeverGoNegative`. The DB also rejects negative balances and drift between balance and transactions at commit. |
| RG tests | `internal/rg`: cooling-off on increases, a new request voids a pending raise, time-outs cannot be shortened, 6m/1y/permanent self-exclusion, Nairobi window boundaries, deposit-limit check, restrictions append-only. |
| Auth / IDOR tests | `internal/api`: every authenticated route refuses missing, garbage, foreign-signed and logged-out tokens; every write needs an `Idempotency-Key`; no route accepts a user id (bodies with `user_id` are rejected, per-id routes 404); B cannot see A's KYC or limits. Mutation check: removing `requireAuth` makes the auth test fail. |
| ERD | `docs/erd.svg` |

### Brief invariants covered so far (section 3)

2 money is bigint (`TestSchema_MoneyIsIntegerAndTimeIsTimestamptz`, integer-only `ledger.Apply`); 3 no negative balance;
4 balanced journals and wallet = sum of transactions (deferred triggers + property test); 5 append-only ledger, wallet
transactions, audit log (grants and triggers that also stop owners); 6 play cap in the database (`tickets` CHECK +
UNIQUE); 7 idempotency (middleware and unique business keys, ADR 0003); 10 time from an injectable clock (lint bans
`time.Now`); 12 eligibility checks exist (`identity.AssertEligible`, `rg.AssertNotRestricted`) and are not yet called by
any money path because top-ups and plays arrive in phases 2 and 3. Invariants 1, 8, 9, 11, 13 belong to later phases.

### Deviations and honest notes

- **TDD order:** the HTTP handlers in `internal/api` were written before their tests (tests followed in the same PR and
  were mutation-checked). The `PORT` fallback in config was added in the Render fix without a test; the test was added
  in this phase. Everything else was test-first with the red run observed.
- **Pre-push flake:** one `make test-int` run in the pre-push hook failed once and passed on rerun with no change;
  the cause was not identified. Watch for it before trusting the integration suite as non-flaky.
- **Schema deviations** from the brief are recorded in ADR 0004 (idempotency principal, rerun-able draws, `user_id` on
  ledger entries, extra wallet transaction kinds, `rg.limits.cancelled_at`).
- **Handlers live in `internal/api`,** not in each module, so modules stay HTTP-free.
- **Client attestation is never trusted:** the field is not accepted and every device is stored as `none` until the
  server-side Play Integrity / App Attest verifier exists (phase 7). Failed attestation therefore cannot yet block play.

### Deferred (not done in Phase 1)

- **`/v1` is not mounted in `cmd/api`.** It needs secrets (OTP key, token key, KEK), Valkey and an SMS adapter that do
  not exist in the deployment yet, so the Render service still serves only the health probes. Wiring lands with phase 2.
- **KYC runs synchronously** inside `Submit`; the brief asks for a River job, which arrives when River is introduced
  (phase 2). No real SMS, KYC or Google/Apple production configuration exists, only the interfaces, mocks and the real
  JWKS verifier (tested against a local JWKS server).
- **Database role provisioning:** roles are created `NOLOGIN` and objects are owned by `app_migrator`; creating login
  roles and passwords, and whether Render's database user may run `ALTER ... OWNER TO app_migrator`, must be verified
  when the Blueprint is finalised (phase 8).
- **Mutation testing (gremlins)** has not been run against the new packages; the nightly job has not been exercised.
- Keycloak/staff auth, device-bound push, the realtime WebSocket and the Valkey session store are untouched.

### DECISION items touched
Device-to-account cap 2, deposit-limit cooling-off 24 h, OTP expiry 5 min / 5 attempts / 60 s resend, access token
10 min, refresh token 30 days (placeholder), idempotency key lifetime 24 h (placeholder). No tax rates were invented:
with no `tax_rules` version in force the store returns `ErrNoTaxRule`. `TRUSTED_PROXY_HEADER` (the header Render's load
balancer sets) must be configured before the API is exposed, or per-IP limits would key on the balancer's address.


## Addendum: deployment wiring and hardening (after the phase 1 review)

Closed the items deferred above:

- **/v1 is wired into `cmd/api`** through `internal/app`, which both `cmd/api` and the new `cmd/worker` use. It is all-or-nothing:
  none of the API secrets set means health probes only; some but not all set means the process refuses to start and
  names what is missing. `TestApp_FullJourneyThroughTheRealWiring` runs SMS, OTP sign-in, KYC submission and worker
  verification through the real wiring; `make e2e` does the same against the compose stack (SMS and registry mocks).
  Anything unconfigured fails closed (no SMS gateway: OTP unavailable; no registry: KYC retries then abandons; no
  client id: SSO refused).
- **KYC is a River job.** `Submit` stores an encrypted `pending` check, marks the account `pending` and enqueues the job in
  the same transaction; `Process` verifies with the registry; a provider outage retries with backoff and, after the last
  attempt, `Abandon` frees the player to resubmit. Personal data never enters the job table. The API now answers
  `202 {"status":"pending"}` and the app polls `GET /v1/kyc`. River's tables live in the `river` schema.
- **Database roles on a managed Postgres.** Verified by running the migrations as a non-superuser, with and without
  `CREATEROLE`: the original migrations would have **failed** on Render. They now create roles when allowed (using
  `GRANT ... WITH SET TRUE`, required on PostgreSQL 16+) and otherwise run in a warned single-role mode where every
  trigger and constraint still holds. What could not be verified from here is which of the two the real Render user
  gets: `docs/runbooks/database-roles.md` has the two queries to run.
- **Trusted proxy.** `TRUSTED_PROXY_HEADER` is mandatory once the API is enabled; for `X-Forwarded-For` the client is the
  N-th entry from the right (`TRUSTED_PROXY_HOPS`), so client-invented entries are never used. Render does not document
  its behaviour, so the Blueprint uses the safe default (rightmost entry) and `docs/runbooks/trusted-proxy.md` has the
  two post-deploy probes that must be run before the API is exposed.
- **Attestation.** The client sends only an opaque token; a server-side `Attestor` decides the verdict (stored per device),
  `AssertDeviceTrusted` refuses failed devices (and unattested ones in strict mode) and the default verifier never
  passes anyone. The real Play Integrity / App Attest verifiers need Google and Apple credentials and real devices and
  are not built: until then every mobile device is `none`, and strict mode must stay off.
- **Render Blueprint:** free Postgres + free Key Value + one web service that migrates on start (advisory-locked) and runs
  workers in-process; generated secrets; vendor keys `sync: false`. A test keeps `render.yaml` in step with the config.
- Fixed along the way: the OIDC verifier would have panicked on the first Google/Apple sign-in (database clock with no
  database); compose's Postgres healthcheck passed against the init-time server and made migrations flaky; `Wall` clock added.

Still open: mutation testing (`gremlins` could not be installed here because the module proxy timed out; see
`docs/runbooks` once run), real SMS / registry vendors, and the production verification of the two Render behaviours above.
