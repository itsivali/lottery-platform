# 0008. How a paybill (C2B) payment is settled

Status: accepted
Date: 2026-10-24

## Context
Players can also pay into the paybill from the M-Pesa menu, typing an account number. Owner decisions (2026-10-22):
the account number is the player's registered phone number; only a *proved* phone matches (an unproved number can be
claimed by several accounts, ADR 0005); the sandbox test shortcode is 600982 while STK stays on 174379. Daraja may
call a validation URL before the payment and always posts a confirmation after it. Like an STK callback, a
confirmation is a claim that anyone who learns the URL can forge (ADR 0006). Daraja's Transaction Status API answers,
asynchronously, whether a receipt completed and for how much, but not which account number was typed.

## Decision
- **Validation** (`/hooks/c2b/{secret}/validate`) accepts only a payment for our shortcode, of a whole amount within
  the top-up bounds, whose account number is the proved phone of an active, KYC-verified, unrestricted player within
  their deposit limits. It writes nothing and refuses with coarse codes (C2B00012 account, C2B00013 amount, C2B00015
  shortcode, C2B00016 anything else, including a failure to decide). URLs are registered with
  `ResponseType=Cancelled`: when our validation cannot be reached the payment is refused rather than taken.
- **Confirmation** (`/confirm`) is stored once per receipt in `payments.c2b_confirmations` and moves no money. A later
  confirmation for the same receipt that disagrees (account, amount, payer) marks the payment `conflicting`.
- **Verification**: a River job asks Transaction Status (the receipt goes in the request's occasion so the answer can
  be matched even when it names no receipt), waits for the answer at `/status-result`, and asks again on silence or
  queue timeouts, up to 10 times, then leaves the payment `unverified`. Asking again is harmless: the query moves no
  money. A refused request (bad initiator or credential) also leaves it `unverified`; `lotteryctl payments
  c2b-recheck` sends it through again once fixed.
- **Settlement** from the answer: no completed transaction, another receipt, or money credited to another shortcode →
  `unverified` (nothing posted). Otherwise the money is real and **Daraja's amount counts**. It is credited through the
  normal credit path (excise deducted, debt recovered, receipt used once) as a `payments.payments` row with channel
  `c2b`, unless it is conflicting, the amounts or a visible payer number disagree, or the account does not match an
  eligible player: then it goes to **suspense** (`payments.c2b_unmatched` with a reason; ledger `MPESA_CLEARING` →
  `SUSPENSE`).
- **A paybill payment never proves a phone**: anyone can pay into any account number.
- **Suspense** is settled by an operator: `suspense-credit` releases it (`SUSPENSE` → `MPESA_CLEARING`) and credits an
  eligible player as an ordinary deposit; `suspense-refund` records the M-Pesa refund reference and releases it.
  Reconciliation now checks `MPESA_CLEARING` = credited + open suspense, `SUSPENSE` = open items, and that every item
  has its journals; open items, unverified and overdue paybill payments are listed for an operator.
- The hooks share the STK callback's secret segment under `/hooks/c2b/` because C2B registration refuses URLs that
  contain "mpesa". The initiator's password is configured already encrypted (`DARAJA_SECURITY_CREDENTIAL`, from the
  Daraja portal), so no certificate is handled by the app.

## Consequences
- The secret URL is what binds a confirmation to an account number; Transaction Status cannot. A forger who knew both
  the URL and a real, not yet settled receipt could have that payment credited to an account of their choosing if
  their confirmation is the only one seen before settlement; a disagreeing second confirmation sends it to suspense
  instead, and one that arrives after the credit is audited and listed by reconciliation (`c2b_conflict_after_credit`).
  They can never credit more than Daraja confirms, or anything Daraja did not see paid. Treat a leak of
  `DARAJA_CALLBACK_TOKEN` as an incident: rotate it and register the URLs again.
- A payment Daraja will not confirm waits as `unverified` instead of being credited or lost; the statement decides.
- Production needs a real paybill, its own initiator, and one-time URL registration (Safaricom must clear URLs to
  change them). Matching against the M-Pesa statement remains to be built.
