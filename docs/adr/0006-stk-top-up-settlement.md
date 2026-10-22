# 0006. How an STK top-up is settled

Status: accepted
Date: 2026-10-22

## Context
An STK Push top-up crosses three unreliable boundaries: our push request (which can time out after Safaricom accepted
it), Daraja's callback (which can be lost, duplicated, delayed or forged by anyone who learns the URL) and the status
query (which answers "still processing" for a while, sometimes as HTTP 200 with result code 4999). The same top-up
also proves the player's phone (ADR 0005), so a mistake either loses money, credits money that was never paid, or
proves a phone the player does not hold.

## Decision
- **A callback is a claim; the status query is the proof.** Money moves only when the query says paid. The amount and
  receipt come from the stored callback (the query does not return them), so a paid query with no callback credits
  nothing: after the expiry window the payment becomes `unconfirmed` for reconciliation against the M-Pesa statement.
- **The push is never retried.** A timeout or 5xx may still have reached the customer; a second prompt could take a
  second payment we would not know about. The payment fails (`push_outcome_unknown`) and any callback it later
  produces is kept in `payments.stk_orphan_callbacks`.
- **Exactly-once credit**: payment row lock + final-state check, the unique receipt, the deposit journal keyed on the
  payment id, and a receipt already credited on another payment is refused (`receipt_reused`, unconfirmed).
- **The phone proved is the push target** (the number on the account, never one the client sends). A payer number the
  callback reports that differs from it is treated as suspicious and proves nothing; a masked or missing one is
  ignored. A failure to prove (number already proved by another account) never undoes the credit; it runs in a
  savepoint.
- **Verification jobs are unique per (payment, source)** — `push`, `callback`, `sweep` — so duplicate callbacks and
  repeated sweeps collapse, while a callback is never held back behind a snoozed safety-net job.
- **The callback URL's secret segment is sha256(DARAJA_CALLBACK_TOKEN) in hex**, because a Render-generated base64 secret
  may contain `/`. The route lives outside `/v1` (no token, no Idempotency-Key) and compares in constant time.
- **Limits count top-ups in flight**, under a per-player advisory lock, so parallel requests cannot pass a deposit limit.
- Excise is deducted from the deposit (owner, 2026-10-22) at the rate version in force when the credit is posted.

## Consequences
- A lost callback delays a real payment until reconciliation (slice 4) supplies the receipt. Players see `pending`.
- A push outage costs the player a retry rather than risking a double charge.
- Without Daraja credentials or an excise version in force, top-ups are refused (503) rather than credited untaxed.
