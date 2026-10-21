# 0005. Password accounts; phone ownership proved by an M-Pesa confirmation, not an SMS code

Status: accepted (WhatsApp code under discussion, see Consequences)
Date: 2026-10-21

## Context
The brief signs players up with a phone number verified by a 6-digit SMS OTP. SMS costs money per message, the owner does not
want recurring messaging costs, and the product already takes M-Pesa payments from the same number.

## Decision
- An account is created with a phone number and a password (argon2id, NFKC-normalised, HMAC-peppered, 8-128 characters,
  deny-list, rate-limited per number and per IP). There is no SMS and no OTP table.
- A number is only a **claim** until the player proves control of it. Proof is the payer number named in an M-Pesa STK Push
  confirmation, verified with Daraja's status query, during the first top-up (phase 2 calls `MarkPhoneVerified`). Uniqueness
  is enforced only for verified numbers (partial unique index), so a squatter cannot lock out the real owner; once the owner
  proves the number, other claims can never verify and cannot log in.
- Eligibility is split: **top-up** needs a verified national ID and an active account; **play** additionally needs the phone
  proved. Linking Google/Apple needs both.
- TOTP and passkeys are deferred; recovery/reset of a forgotten password will require a fresh STK proof plus an ID match.

## Consequences
- Departs from the brief's SMS OTP; the regulator should confirm it accepts this.
- There is no password reset yet. Until phase 2 there is no way to prove a phone, so nobody can play or withdraw in production.
- Sign-up cost is zero. If a messaging channel (for example WhatsApp) is added later it becomes one more way to call
  `MarkPhoneVerified`, not a new account model.
