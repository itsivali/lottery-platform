# 0003. Idempotency: HTTP middleware plus unique business keys

Status: accepted
Date: 2026-10-21

## Context
Invariant 7 requires exactly-once money movement and an `Idempotency-Key` on every write. An HTTP-level claim table
alone cannot give exactly-once: the handler's transaction and the stored response are separate commits, so a crash
between them lets a retry re-run the handler.

## Decision
- `platform.idempotency_keys` is keyed by `(principal, key)` where principal is `user:<uuid>` (or `anon:<id>` for
  unauthenticated calls). The brief's `PK (user_id, key)` cannot hold anonymous callers; this is the recorded deviation.
- The middleware claims the key, runs the handler, then stores status, body, content type and `Location` (no other
  headers). Same key and request replays; same key and a different method/path/query/body gives 422; a concurrent
  duplicate gets 409 with `Retry-After`. 5xx responses and panics release the claim so the client can retry.
- An unfinished claim older than 60 s may be taken over, and expired keys (24 h) can be reused.
- Exactly-once is enforced by the database, not the middleware: `tickets (user_id, idempotency_key)`,
  `payments (user_id, idempotency_key)`, `payouts` keys, `payments.mpesa_receipt`, and the unique
  `(kind, reference_type, reference_id)` on ledger journals. A stale takeover or crash therefore cannot double-apply.

## Consequences
A crash after commit but before response storage means the retry re-enters the handler, which then hits the business
unique key and returns the already-created result. Handlers must treat a unique-violation on their business key as
"already done", never as a failure.
