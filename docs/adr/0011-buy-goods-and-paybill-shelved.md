# 0011. Top-ups can pay a till (Buy Goods); paybill (C2B) is shelved

Status: accepted
Date: 2026-10-24

## Context
The paybill path (ADR 0008) is built and deployed, but the Daraja sandbox has never called its hooks back, and there
is no production paybill. The owner decided (2026-10-24) to **shelve the paybill** until one is available and to
take money in through **Buy Goods** instead. The owner first asked for Buy Goods "to a phone number" (0796992457);
Daraja cannot send money to, or confirm money received by, a plain phone number or a Pochi la Biashara, so the owner
chose STK Buy Goods into a till.

## Decision
- `DARAJA_TILL_NUMBER` (optional): when set, the STK prompt is `CustomerBuyGoodsOnline` with the till as `PartyB`;
  `DARAJA_SHORTCODE` is then the till's store (head office) number, which signs the request and the status query.
  Unset keeps the paybill prompt (`CustomerPayBillOnline` into `DARAJA_SHORTCODE`). A till is 5 to 8 digits; a phone
  number is refused at start-up.
- Nothing after the push changes: the callback is a claim, the status query is the proof, the credit is the confirmed
  amount net of excise, exactly once, and the approved prompt proves the player's phone (ADR 0005, 0006).
- Paybill (C2B) code, hooks and operator actions stay in place, unused while `DARAJA_C2B_*` are unset; no more work on
  it until a paybill exists.

## Consequences
- Production needs a till with Lipa na M-Pesa Online (STK) enabled on Daraja, its store number and its passkey
  (owner). A Pochi la Biashara has to be converted to a till for that.
- Players can still only pay through the prompt we send; paying the till from the M-Pesa menu (without a prompt) is
  not detected (that would need C2B URLs on the till, the shelved path).
