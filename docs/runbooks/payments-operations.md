# Runbook: payments operator actions

Until the back office exists, operators use `lotteryctl` with the database URL (the external URL from the Render
dashboard; never commit it). Every action is audited as staff `operator:<name>`.

```sh
export DATABASE_URL='postgres://...'          # external connection string of lottery-db
go run ./cmd/lotteryctl payments reconcile    # prints the report; exit 1 if there are mismatches
```

## The daily reconciliation

Runs in the api's job workers every 24 hours (and on start); each run is stored in `payments.reconciliation_runs` and
logged: `ERROR payments reconciliation found mismatches` or `WARN ... items need an operator`.

- **Mismatches** break an invariant between payments, ledger and wallets (`credited_without_deposit`,
  `deposit_without_credit`, `deposit_amount_mismatch`, `reversal_mismatch`, `clearing_vs_credited`,
  `liability_vs_wallets`). They should never happen: stop and investigate (a bug or tampering).
- **Attention** lists work for an operator:
  - `unconfirmed`: the status query said paid but no receipt arrived. Find the payment on the M-Pesa statement (same
    phone number, amount, time) and resolve it (below). If it is not on the statement, leave it and note why.
  - `overdue_pending`: pending past the expiry window: the sweep should settle these; persistent ones are a bug.
  - `orphan_callbacks`: callbacks for pushes we never recorded. Check the statement for a matching receipt.
  - `players_in_debt`: players who owe after a reversal; recovered automatically from their next top-ups.

## Resolve an unconfirmed top-up from the statement

```sh
go run ./cmd/lotteryctl payments resolve -payment <payment id> -receipt <M-Pesa receipt> -amount <KES on the statement> -operator <your name>
```

Credits exactly like a confirmed callback (excise deducted, debt recovered, phone proved). Refused if the payment is
not `unconfirmed` or the receipt already credited another payment.

## Reverse a top-up M-Pesa took back

```sh
go run ./cmd/lotteryctl payments reverse -receipt <M-Pesa receipt> -reason "<why>" -operator <your name>
```

Takes back what the player received (ADR 0007): from the wallet as far as it goes, the rest as debt. Refused unless
the payment is `credited`; a payment can be reversed once.
