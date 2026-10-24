# Status

Read this first at the start of every session.

- **Phase:** 1 — Foundations: complete. **Phase 2 — Payments: in progress** (`docs/checkpoints/phase-2.md`). Slices 1
  (Daraja client + mock), 2 (STK top-up, ADR 0006) and 4 (reversals, player debt, internal reconciliation, ADR 0007)
  are merged and live. **Slice 3 (C2B paybill, ADR 0008) is merged and live (2026-10-24, #18, #19)**; its sandbox
  end-to-end run is blocked on Daraja: the sandbox accepts registrations, simulations and status requests but has not
  called back yet (below). Next: slice 5 (realtime).
- **Repo state:** the GitHub repository was **deleted and recreated on 2026-10-22** (to drop co-author attribution that
  GitHub kept in old PR refs and caches) and `main` was pushed from the local clone. PR numbers below #8 in these docs
  refer to the deleted repository. Commits and PRs carry only the owner's name.
- **Deployed (verified 2026-10-24):** Blueprint `lottery-platfform`; `lottery-api` (`https://lottery-api-7jmj.onrender.com`,
  free, Oregon) builds from `main` (repository reconnected by the owner on 2026-10-24); migrations at version 17;
  sandbox top-ups and paybill on (`payments=true paybill=true`, shortcode 600992); client address from
  `CF-Connecting-IP`. Free Postgres `lottery-db` (PG 18) and Key Value `lottery-cache`.
- **Keys:** `DATA_KEY` and `IDENTITY_KEY` were handed to the owner for the password manager; save `AUTH_KEY` too.

## Before the API is exposed to players

1. **Trusted proxy: fixed.** Measured on the live service (`docs/runbooks/trusted-proxy.md`): the rightmost
   `X-Forwarded-For` entry was a rotating Render balancer address; the api now reads `CF-Connecting-IP`, which
   Cloudflare refuses to accept from clients. **Probe 1 passed** (60 × 401 then 429 with forged `X-Forwarded-For`).
   Remaining: **probe 2** from a second network (5 failed logins from a phone on mobile data must all be 401, not 429).
2. **Free Postgres:** rotated on 2026-10-22 (first supervised run, data intact); now expires **2026-11-21** and the next
   rotation runs by itself around 2026-11-18. **Automated** by `db-rotate` (`docs/runbooks/free-database-rotation.md`): daily
   check, rotation (backup, verify, recreate, restore, switch) when 3 days or fewer remain. Secrets are set; the
   passphrase must stay in the owner's password manager. Upgrading the database to a paid plan ends the need for it.
3. **Database roles:** Render's user has `CREATEROLE`; the original instance had the four app roles. Since the rotation
   the instance runs in **single-role mode** (no `app_*` roles; every trigger and constraint holds), as documented.

## Decisions taken (see ADR 0005)

- Accounts are created with **phone number + password** (argon2id, peppered). **No SMS, no OTP, no TOTP for now.**
- Phone ownership is proved by the **M-Pesa STK Push confirmation at the first top-up** (Phase 2 calls
  `identity.Service.MarkPhoneVerified`). The proof is the push target (the phone that approved the prompt); the
  callback's payer number is only cross-checked when present, because Safaricom may mask it.
- **KYC is reviewed by hand (owner, 2026-10-24, ADR 0009):** no registry provider; submissions wait in `manual_review`
  for `lotteryctl kyc approve|reject` (back office later). Google/Apple sign-in ids come with the web app; SSO is off.
- Free-tier deployment: migrate on start (advisory lock), workers in-process, free Postgres + Key Value.
- **Deposit excise (owner, 2026-10-22): deducted from the deposit** (the wallet shows the deposit and a separate excise
  line). Rate version to load: `deposit_excise` 5% `half_up` effective 2025-07-01 (owner's choice, based on the Finance
  Act 2025; to be confirmed with a tax adviser before production).
- Daraja: **sandbox only** (shortcode 174379). The owner's sandbox consumer key/secret and passkey work: a live KES 1
  push to the owner's test phone went pending (result 4999) and then paid (2026-10-22). They go into Render as
  `sync: false` variables, never into git.

## Slice 3 (C2B) decisions (owner, 2026-10-22)

- Sandbox C2B test shortcode **600992** (the Party A on the owner's Test Credentials page; changed from 600982 on 2026-10-24). STK Push stays on 174379. Production needs a real paybill later.
- **Account number = the player's registered phone number.** Only a *proved* phone matches (an unverified number can
  be claimed by several accounts); anything else goes to suspense for an operator.
- URLs `/hooks/c2b/{secret}/validate` and `/confirm` (C2B URL registration refuses paths containing "mpesa").
  Validation rejects unknown accounts, restricted players, deposits over a limit and bad amounts. A confirmation is
  stored and verified with Daraja's **Transaction Status API** before it is credited (ADR 0006), through the same
  credit path as STK top-ups.
- **Built (2026-10-24, ADR 0008):** validation, confirmation, Transaction Status verification, credit or suspense,
  operator actions (`lotteryctl payments suspense-credit | suspense-refund | c2b-recheck | c2b-register |
  c2b-simulate`), reconciliation of suspense. Paybill payments never prove a phone.
- **Needs from the owner:** the sandbox **initiator name** and its **security credential** (Daraja portal, Test
  Credentials page: the portal encrypts the initiator password into the credential). Then set `DARAJA_C2B_SHORTCODE=600992`,
  `DARAJA_INITIATOR_NAME`, `DARAJA_SECURITY_CREDENTIAL` on Render, deploy, run `lotteryctl payments c2b-register`, and
  simulate a payment to the test account (`c2b-simulate`).

## Owner items open at the end of the session

- **Paybill sandbox run:** URLs registered for 600992 (v1 and v2), payments simulated to the test account, and a
  Transaction Status request accepted with the owner's initiator credential, but Daraja has not called the hooks
  (they answer correctly from outside). Retry; if it persists, raise it with Safaricom.
- PR "release 1.0.0" (release-please) is **held** by the owner's decision; do not merge.
- The nightly job failed every night until #20 (2026-10-24): the property step tested the empty root package. It now
  passes (mutation: wallet 100%, ledger 97.7%, payments 89.7%).
- Trusted-proxy probe 2 from a second network; rotate the Render API key and the Daraja consumer secret.

## Next steps

1. **Recreate `lottery-api`** from the Blueprint (the owner deleted the service on 2026-10-24 to fix auto-deploy; the
   database and Key Value survived). Then set the Daraja and paybill values again, update the service id and URL in
   `db-rotate.yml`, `deploy-prod.yml` and the `RENDER_API_SERVICE_IDS` secret, and register the paybill URLs again.
   The generated keys are new, so the test account (0796992457) signs up again and goes through manual KYC review.
2. Phase 2 slice 5: realtime confirmation of top-ups.
3. Owner: run trusted-proxy probe 2 from a second network; define the manual KYC review procedure and service level.
4. Later: password reset (STK proof + ID match), passkeys/TOTP, real Play Integrity / App Attest verifiers.

## Open questions for the owner

- Confirm the 5% deposit excise version with a tax adviser before production.
- Does GRA accept phone verification by M-Pesa confirmation instead of SMS OTP (ADR 0005), and manual identity checks
  instead of a registry lookup (ADR 0009)?
- Minimum verification deposit amount (suggested KES 10) and excise treatment on it.
- `internal/notifications` (SMS sender) is unused since ADR 0005: keep for a future channel or delete?
