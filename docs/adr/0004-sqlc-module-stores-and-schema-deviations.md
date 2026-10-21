# 0004. sqlc stores per module, and deliberate schema deviations from the brief

Status: accepted
Date: 2026-10-21

## Context
The brief requires sqlc (no string-built SQL) and one schema per module. A few table shapes in the brief cannot hold
in practice or need an extra column to keep an invariant checkable.

## Decision
- Queries live in `db/queries/<module>.sql`; `sqlc.yaml` generates a `store` package beside each module, with uuid
  mapped to `string` so modules never share pgx types. `make gen` falls back to `go run sqlc` when it is not on PATH.
- Deviations: (1) `draws` uniqueness is `UNIQUE (draw_type, period_start) WHERE status <> 'voided'` so a voided draw can
  be re-run into a new row (`rerun_of`); (2) `ledger_entries.user_id` records which player a wallet-liability entry
  belongs to so each wallet reconciles against the ledger on its own; (3) `wallet_transactions` has kinds
  `deposit_excise` and `reversal` so excise is shown to the player and reversals are explicit; (4) the idempotency
  principal (ADR 0003).
- Invariants are enforced by deferred constraint triggers (journal balance, wallet = sum of transactions), append-only
  triggers that fire for owners and superusers as well as grants, and a one-time-seed trigger on `draws`.

## Consequences
Migrations stay additive (expand now, contract later). Anything that must hold regardless of application bugs lives in
the schema, and each such rule has an integration test against real Postgres.
