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
  `deposit_without_credit`, `deposit_amount_mismatch`, `reversal_mismatch`, `clearing_vs_credited` (now credited +
  open suspense), `liability_vs_wallets`, `suspense_vs_items`, `suspense_without_journal`). They should never happen: stop and investigate (a bug or tampering).
- **Attention** lists work for an operator:
  - `unconfirmed`: the status query said paid but no receipt arrived. Find the payment on the M-Pesa statement (same
    phone number, amount, time) and resolve it (below). If it is not on the statement, leave it and note why.
  - `overdue_pending`: pending past the expiry window: the sweep should settle these; persistent ones are a bug.
  - `orphan_callbacks`: callbacks for pushes we never recorded. Check the statement for a matching receipt.
  - `players_in_debt`: players who owe after a reversal; recovered automatically from their next top-ups.
  - `suspense_open`: paybill payments Daraja confirmed that could not be credited (see below).
  - `c2b_unverified`: paybill confirmations Daraja did not confirm. Check the statement; if the payment is there,
    fix the cause (initiator, credential) and run `c2b-recheck`.
  - `c2b_overdue`: paybill confirmations still being verified after an hour: Transaction Status is not answering.
  - `c2b_conflict_after_credit`: a second, different confirmation arrived for a receipt already credited. Possible
    forgery: compare with the statement, reverse if credited to the wrong player, and rotate `DARAJA_CALLBACK_TOKEN`.

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

## Paybill (C2B) payments

Paybill payments are verified with Daraja's Transaction Status API before anything is credited (ADR 0008). Each one
ends `credited`, in **suspense**, or `unverified`. The reason is in `payments.c2b_confirmations.outcome_reason` and,
for suspense, `payments.c2b_unmatched.reason`: `unknown_account` (the account number is no proved phone),
`kyc_required`, `account_suspended`, `restricted` (time-out or self-exclusion), `over_deposit_limit`, `bad_amount`,
`amount_mismatch`, `payer_mismatch`, `conflicting_confirmations` (two confirmations disagreed: possible forgery).

### Register the paybill URLs (once per shortcode)

Needs the api's Daraja variables, the three C2B variables, `DARAJA_CALLBACK_TOKEN` and `PUBLIC_BASE_URL` (copy them
from the Render dashboard into your shell; never commit them):

```sh
go run ./cmd/lotteryctl payments c2b-register
```

The sandbox accepts repeated registrations. In production a shortcode registers once; changing URLs needs Safaricom.

### Credit a suspense item to the right player

After identifying the payer (for example they call support and prove the account is theirs):

```sh
go run ./cmd/lotteryctl payments suspense-credit -receipt <M-Pesa receipt> -user <user id> -operator <your name>
```

Refused for players who may not receive it (KYC, suspended, time-out or self-exclusion, deposit limit): refund those.
It credits like any deposit (excise deducted, debt recovered) and never proves a phone.

### Refund a suspense item

Send the money back from the M-Pesa org portal (or a B2C payment), then record it with that transaction's reference:

```sh
go run ./cmd/lotteryctl payments suspense-refund -receipt <M-Pesa receipt> -refund-ref <refund reference> -operator <your name>
```

Self-excluded and timed-out players' payments must be refunded, not credited.

### Check an unverified paybill payment again

```sh
go run ./cmd/lotteryctl payments c2b-recheck -receipt <M-Pesa receipt> -operator <your name>
```

The worker asks Daraja again within a minute or two.

### Sandbox end-to-end

```sh
go run ./cmd/lotteryctl payments c2b-simulate -amount 10 -msisdn 254708374149 -account <test player's proved phone>
```

The sandbox only pays from its test number; the account number decides who is credited.
