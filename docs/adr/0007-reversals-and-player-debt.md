# 0007. Reversals take back what the player received; the shortfall is per-player debt

Status: accepted (excise treatment pending a tax adviser)
Date: 2026-10-22

## Context
M-Pesa can reverse a top-up after it was credited (a dispute upheld by Safaricom). By then the player may have spent
it. With deposit excise deducted (owner, 2026-10-22), a KES 100 top-up credited the player KES 95; the phase 1
reversal rule clawed back the full 100, charging the player the excise a second time. Debt was booked without a user,
so one player's debt could not be totalled or recovered.

## Decision
- A reversal takes back **what the player received** (gross − excise deducted): from the wallet as far as the balance
  goes, the rest as **player debt** booked against that player (`PLAYER_DEBT` entries carry the user).
- M-Pesa takes back the gross (`MPESA_CLEARING` credited in full). The excise already owed to KRA is booked as the
  operator's cost (`EXCISE_EXPENSE`); `EXCISE_PAYABLE` is not reduced, because whether excise is refundable on a
  reversed deposit is a tax question not yet answered.
- Debt is recovered first from every later deposit (`debt_recovery`, keyed on that deposit, in the same transaction
  as the credit). Play and withdrawals must refuse a player with debt (`wallet.Debt`), enforced when those phases land.
- Reversals and manual resolution of `unconfirmed` top-ups are operator actions (`lotteryctl payments reverse|resolve`),
  audited as staff, until the back office exists (phase 6).
- A daily internal reconciliation checks payments against the ledger and wallets and stores each run. Matching against
  the M-Pesa statement needs the paybill and comes with C2B.

## Consequences
- If the tax adviser says excise is refundable on reversals, a later posting moves it from `EXCISE_EXPENSE` back
  against `EXCISE_PAYABLE`; nothing already posted changes.
- A player in debt can still top up (that is how the debt is repaid) but cannot play or withdraw.
