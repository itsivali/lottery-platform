# Phase 2 — Payments: checkpoint

Status: IN PROGRESS (started 2026-10-22 on the owner's go-ahead). Slices 1 (#26), 2 (#27), 4 (#13) and 3 (#18, #19)
merged and live; slice 5 (realtime, ADR 0010) built. The first sandbox top-up from the owner's re-created test
account was credited and watched over the stream (2026-10-24). Paybill shelved, Buy Goods added (ADR 0011).

## Plan (written before coding)

Scope: Daraja client and fault-injecting mock, STK top-up (which is also the phone-ownership proof, ADR 0005), C2B,
verification via the status query, reconciliation, suspense, reversals, excise posting, realtime confirmation.

Rules that hold across every slice:

- **A callback is a claim, never proof.** Money moves only after `STKQuery` confirms the result; the credited amount is the
  confirmed amount from the callback, never the requested one (invariant 8, already a CHECK on `payments.payments`).
- **Exactly once.** `payments.mpesa_receipt` and `checkout_request_id` are unique, the deposit journal is keyed on the
  payment id, and the credit, the wallet transaction and `MarkPhoneVerified` commit in one transaction.
- **No network call inside a database transaction.** Pushes and queries run in River jobs; the request path only
  records intent and enqueues.
- **Fail closed.** Without Daraja configuration, or without an excise rule in force, top-ups are refused (503), never
  credited untaxed. A production Daraja base URL is refused unless explicitly switched on.
- **Sandbox only** until the owner says otherwise. Credentials live in Render (`sync: false`) and the operator's password
  manager, never in git.

### Slice 1 — Daraja client + mock (this PR)
`internal/payments/daraja`: OAuth with token caching and one forced refresh on 401, STK Push, STK query (pending /
paid / failed, "being processed" mapped to pending), callback parsing that refuses fractional, missing or zero amounts
and ignores masked payer numbers. Errors are either `ErrUnavailable` (retry) or `ErrRejected` (do not), and never contain
credentials, tokens or phone numbers. `internal/payments/darajamock` + `mocks/daraja`: the same contract in memory,
pending until completed (deterministic tests) or auto-completed (compose), with one-shot faults: push 4xx/5xx, query
outage, cancelled/failed result codes, lost and duplicated callbacks, a different paid amount, masked phone.

### Slice 2 — STK top-up end to end
Config (`DARAJA_*`, callback path token; excise is deducted per the owner's decision), `POST /v1/payments/topups` (eligibility via
`AssertCanTopUp`, RG deposit limit, amount bounds, idempotency), `stk.push` job, public callback endpoint (secret path
token, raw body stored, always 200 to Daraja), `stk.verify` job (query → credit via `ledger.Deposit` + `wallet.Apply`
+ `MarkPhoneVerified` in one transaction), expiry sweeper for pushes that never resolve, `GET /v1/payments/{id}`.
Tests: every mock fault above through the real wiring; concurrent duplicate callbacks credit once; a different paid
amount credits what was paid; a callback for a number that another account already proved credits but does not verify.

### Slice 3 — C2B
Validation/confirmation URLs registered against the paybill; account reference → verified player; unmatched receipts
go to `payments.c2b_unmatched` and the `SUSPENSE` account; back-office resolution later. Owner decisions
(2026-10-22): sandbox shortcode 600982 (600992 from 2026-10-24, the test credentials' Party A), account number = the registered phone, only a proved phone matches, confirmations
verified with the Transaction Status API before crediting.

### Slice 4 — Reconciliation and reversals
Reversal handling via `wallet.ReverseDeposit` (player debt suspends play and withdrawal), manual resolution of
`unconfirmed` top-ups, a daily internal reconciliation report. Matching against the M-Pesa statement (and the
Transaction Status API, which needs a receipt) waits for the paybill (slice 3).

### Slice 5 — Realtime confirmation and checkpoint report
WebSocket (or SSE) push of top-up results to the app; e2e journey against compose; mutation testing of `payments`.

## Results

### Slice 1
- `internal/payments/daraja`: 90.2% coverage; `internal/payments/darajamock`: 87.9%. Lint clean, race-clean.
- The owner's sandbox app credentials obtain an OAuth token from `sandbox.safaricom.co.ke` (checked 2026-10-22).
- Deviation: the client tests and code were written together before the first run, so the red run was not observed.
- **Live sandbox round trip (2026-10-22):** a KES 1 STK push to the owner's test phone was accepted, the first status
  query answered HTTP 200 with `ResultCode 4999` ("The transaction is still under processing"), and after the owner
  approved the prompt the query answered `ResultCode 0`. The client had mapped 4999 to *failed*, which would have failed
  payments that customers were still completing; a failing test reproduced it, the client now maps it to pending, and
  the mock can answer pending queries the same way (`PendingAs4999`).
- Also fixed: `TestAllow_WindowSlides` (rate limiter) raced its own first call with a 300ms window; this was the
  unexplained integration flake noted in the phase 1 checkpoint (reproduced 1 in 3 under load; 5/5 after the fix).

### Slice 2
Design decisions are in ADR 0006. What landed:

- Migration `00015`: `unconfirmed` payment status (paid per the query but no receipt), `result_code`,
  `payments.stk_orphan_callbacks` (append-only), and the `deposit_excise` 5% `half_up` version from 2025-07-01.
- `internal/payments`: `StartTopUp` (eligibility, restrictions, deposit limits counting in-flight top-ups, excise rule
  present, per-player advisory lock, idempotent by key), `Push` (never retried), `HandleCallback` (stores, never
  credits), `Verify` (status query is the proof; credit from the callback's amount and receipt, net of excise; phone
  proved in a savepoint), `Sweep`, River jobs with per-(payment, source) uniqueness and a one-minute periodic sweep.
- `identity.AssertCanTopUp` now returns the claimed number (the push target); `rg.AssertDepositAllowed` checks every
  limit in force against its own Nairobi window.
- API: `POST /v1/payments/topups`, `GET /v1/payments/{id}`, `POST /hooks/mpesa/stk/{path}`; OpenAPI updated and valid.
- Config: `DARAJA_*` all-or-nothing, sandbox default, production refused without `DARAJA_ALLOW_PRODUCTION=true`,
  callback origin from `PUBLIC_BASE_URL` or Render's `RENDER_EXTERNAL_URL`. Blueprint and compose updated; `make e2e`
  now tops up through the Daraja mock.
- Tests (integration, real Postgres): credited net of excise with the phone proved; refused up front (amounts, KYC,
  self-exclusion, limits with in-flight top-ups); unavailable without Daraja or an excise version; same key same
  payment; cancelled prompt; duplicate callbacks plus 10 racing verifications credit once; forged callback not credited
  until the query agrees; lost callback becomes unconfirmed; credited amount is the paid amount; masked payer number;
  number already proved elsewhere is credited but not verified; push outage never retried; receipt replay refused;
  orphan callbacks kept; sweep; IDOR on `GET`. API: auth and idempotency sweeps include the new routes, error mapping,
  secret callback path. App: the full journey (sign-up, KYC, top-up, credited 95, phone proved) through the real River
  workers and an HTTP callback from the mock.
- Found in self-review, fixed test-first: a callback that arrives before the push job has recorded the checkout id is
  stored as an orphan; verification now also looks there, so such a payment is credited instead of waiting 15 minutes
  and ending `unconfirmed`. Also added: a payer number that differs from the push target credits but proves nothing.
- Hand-run mutation check on `internal/payments` (integration tests): 5 of 6 killed (excise mode, receipt-reuse
  check, pending treated as paid, payer-number check, push retried). Survivor: removing the final-state re-check
  inside the credit transaction; a double credit is still impossible there because the deposit journal is unique per
  payment (`ledger_journals_event_key`), so the second attempt errors and retries into the final-state check.

### Slice 4 (no paybill: internal reconciliation and reversals)
Decisions in ADR 0007; operator guide in `docs/runbooks/payments-operations.md`.

- **Reversal rule fixed:** with excise deducted, the phase 1 `DepositReversal` clawed back the gross, charging the
  player the excise twice. It now takes back what the player received (gross − excise); M-Pesa's gross leaves
  `MPESA_CLEARING`; the excise already owed becomes `EXCISE_EXPENSE` (refundability is an open tax question).
- **Per-player debt:** `PLAYER_DEBT` entries carry the user; `wallet.Debt` totals it; `DebtRecovery` takes it back from
  every later deposit in the credit transaction (keyed on that deposit). Play and withdrawals must check `wallet.Debt`
  when those phases land.
- **Operator actions** (`lotteryctl payments reverse|resolve`, audited as staff): reverse a credited top-up by receipt;
  credit an `unconfirmed` top-up from the statement's receipt and amount through the same credit path (excise, debt
  recovery, phone proof, receipt used once).
- **Daily internal reconciliation** (River periodic job + `lotteryctl payments reconcile`, runs stored in
  `payments.reconciliation_runs`): credited ⇔ deposit journal with the confirmed amount, reversed ⇔ reversal journal,
  `MPESA_CLEARING` = credited total, `WALLET_LIABILITY` = wallet total; attention list of unconfirmed, overdue pending,
  orphan callbacks and debtors. Mismatches log at ERROR.
- Migration `00016`: `debt_recovery` wallet kind, `reversed` payment status (with reason and time), per-player debt
  index, `payments.reconciliation_runs` (append-only).
- Tests: ledger (reversal takes back only what was received; debt per player; recovery; property test with random
  excise), wallet (debt recovered once per deposit across deposits), payments (reversal with spend → debt → recovered
  by the next top-up; refusals; resolve exactly once per receipt; reconciliation clean after normal traffic and
  catching a credit the ledger never saw), lotteryctl (parser; reconcile on a fresh database).


### Slice 3 (C2B, paybill)
Decisions in ADR 0008; operator guide in `docs/runbooks/payments-operations.md` (paybill section).

- Daraja client: C2B URL registration (`ResponseType=Cancelled`), sandbox simulation (refused against production),
  Transaction Status requests (receipt echoed in the occasion), and parsers for validation/confirmation bodies and
  status results (whole-shilling amounts only, masked or hashed payer numbers ignored, single-object parameter lists).
- Mock: URL registration that refuses Daraja's forbidden words, simulated payments with optional external validation,
  asynchronous Transaction Status results, and one-shot faults (dropped/duplicated confirmations, masked phone, status
  outage/silence/timeout/failure, different amount, payer or state).
- Migration `00017`: `payments.c2b_confirmations` (one row per receipt, conflict flag, verification state, outcome),
  suspense reason/resolution columns on `payments.c2b_unmatched`. Ledger postings `C2BSuspense`, `C2BSuspenseRelease`,
  `C2BSuspenseRefund`. Identity `ProvedPhoneOwner`.
- Service: validation (no writes), confirmation storage, verification job (`payments.c2b_verify`, swept when stalled),
  settlement (credit through the STK credit path with channel `c2b` and no phone proof, suspense with a reason, or
  unverified), operator actions `suspense-credit`, `suspense-refund`, `c2b-recheck`, plus `c2b-register` and
  `c2b-simulate` in lotteryctl. Reconciliation: clearing = credited + open suspense, suspense account = open items,
  every item journaled; attention lists for open suspense, unverified, overdue and conflicts after credit.
- API: `/hooks/c2b/{secret}/validate|confirm|status-result|status-timeout` (404 unless C2B is configured and the
  secret matches); OpenAPI updated. Config `DARAJA_C2B_SHORTCODE`, `DARAJA_INITIATOR_NAME`,
  `DARAJA_SECURITY_CREDENTIAL` (all or none); Blueprint and compose updated.
- Tests: client and mock unit tests; integration tests for credit net of excise, no phone proof, every suspense reason,
  duplicate confirmations, forged confirmation, conflicting confirmations, Daraja's answer deciding (amount, payer,
  masked payer, failure, reversed), silence → re-ask → unverified, outage retried, other shortcode, unknown result,
  validation codes (and no writes), suspense credit/refund and their refusals, reconciliation of suspense.
- Pending: a live sandbox run (needs the owner's initiator name and security credential), statement matching.

### Slice 5 (realtime confirmation)
Decisions in ADR 0010.

- Migration `00018`: trigger `payments_notify` announces every insert and every change of status or confirmed amount on
  `payments.payments` on the `payment_events` channel (`<user>:<payment>`, sent at commit).
- `payments.Live`: one listening connection per instance (reconnects with backoff), per-player fan-out, resync to every
  subscriber after a reconnection, slow subscribers closed instead of blocking, caps (3 per player, 2000 per instance),
  closed at server shutdown (`RegisterOnShutdown`). `payments.Recent`: the stream's snapshot.
- API: `GET /v1/payments/events` (server-sent events: snapshot, `ready`, `payment` per visible change, heartbeat that
  re-checks the session, `expired`); OpenAPI updated. Internal steps the player cannot see (`stk_sent`,
  `callback_received`) send nothing.
- Tests: Live unit tests (own player only, slow subscriber closed, caps, close); integration (listener connection
  killed → resync → events flow again); API integration (snapshot includes a top-up started before the stream, another
  player's changes never arrive, credit event with the confirmed amount, new payments announced, rolled-back changes
  not announced, 401 without a token, `expired` after logout, 4th stream refused then freed, streams end and new ones
  get 503 at shutdown).
- Not done here: the compose e2e journey does not open a stream yet (it still polls).

### Buy Goods (ADR 0011)
- `DARAJA_TILL_NUMBER` switches the prompt to `CustomerBuyGoodsOnline` into that till (store number signs); the
  settlement path is unchanged. Client, config and mock tests; compose now prompts into a till so `make e2e` covers it.
- Paybill (slice 3) shelved by the owner until a paybill exists.
