# Phase 2 — Payments: checkpoint

Status: IN PROGRESS (started 2026-10-22 on the owner's go-ahead)

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
Config (`DARAJA_*`, callback path token, `DEPOSIT_EXCISE=deduct|absorb`), `POST /v1/payments/topups` (eligibility via
`AssertCanTopUp`, RG deposit limit, amount bounds, idempotency), `stk.push` job, public callback endpoint (secret path
token, raw body stored, always 200 to Daraja), `stk.verify` job (query → credit via `ledger.Deposit` + `wallet.Apply`
+ `MarkPhoneVerified` in one transaction), expiry sweeper for pushes that never resolve, `GET /v1/payments/{id}`.
Tests: every mock fault above through the real wiring; concurrent duplicate callbacks credit once; a different paid
amount credits what was paid; a callback for a number that another account already proved credits but does not verify.

### Slice 3 — C2B
Validation/confirmation URLs registered against the paybill; account reference → verified player; unmatched receipts
go to `payments.c2b_unmatched` and the `SUSPENSE` account; back-office resolution later.

### Slice 4 — Reconciliation and reversals
Daily statement/transaction-status reconciliation against `MPESA_CLEARING`, reversal handling via
`wallet.ReverseDeposit` (player debt suspends play and withdrawal), reconciliation report.

### Slice 5 — Realtime confirmation and checkpoint report
WebSocket (or SSE) push of top-up results to the app; e2e journey against compose; mutation testing of `payments`.

## Results

### Slice 1
- `internal/payments/daraja`: 90.2% coverage; `internal/payments/darajamock`: 87.9%. Lint clean, race-clean.
- The owner's sandbox app credentials obtain an OAuth token from `sandbox.safaricom.co.ke` (checked 2026-10-22; no push
  was sent).
- Deviation: the client tests and code were written together before the first run, so the red run was not observed.
- **Live sandbox round trip (2026-10-22):** a KES 1 STK push to the owner's test phone was accepted, the first status
  query answered HTTP 200 with `ResultCode 4999` ("The transaction is still under processing"), and after the owner
  approved the prompt the query answered `ResultCode 0`. The client had mapped 4999 to *failed*, which would have failed
  payments that customers were still completing; a failing test reproduced it, the client now maps it to pending, and
  the mock can answer pending queries the same way (`PendingAs4999`).
