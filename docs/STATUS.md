# Status

Read this first at the start of every session.

- **Phase:** 1 — Foundations: complete. **Phase 2 — Payments: in progress** (`docs/checkpoints/phase-2.md`). Slices 1
  (Daraja client + mock) and 2 (STK top-up end to end, ADR 0006) are merged and live. **Slice 3 (C2B) waits for a real
  paybill** (owner, 2026-10-22); reconciliation/reversals and realtime confirmation can proceed without it.
- **Repo state:** the GitHub repository was **deleted and recreated on 2026-10-22** (to drop co-author attribution that
  GitHub kept in old PR refs and caches) and `main` was pushed from the local clone. PR numbers below #8 in these docs
  refer to the deleted repository. Commits and PRs carry only the owner's name.
- **Deployed (verified 2026-10-22):** Blueprint `lottery-platfform`; `lottery-api` (`https://lottery-api-7jmj.onrender.com`,
  free, Oregon) builds from `main`; migrations at version 15; sandbox top-ups on (`payments=true`); client address from
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
- KYC runs as a River job. The registry vendor is **not chosen**; Google/Apple developer accounts do not exist yet; both fail closed.
- Free-tier deployment: migrate on start (advisory lock), workers in-process, free Postgres + Key Value.
- **Deposit excise (owner, 2026-10-22): deducted from the deposit** (the wallet shows the deposit and a separate excise
  line). Rate version to load: `deposit_excise` 5% `half_up` effective 2025-07-01 (owner's choice, based on the Finance
  Act 2025; to be confirmed with a tax adviser before production).
- Daraja: **sandbox only** (shortcode 174379). The owner's sandbox consumer key/secret and passkey work: a live KES 1
  push to the owner's test phone went pending (result 4999) and then paid (2026-10-22). They go into Render as
  `sync: false` variables, never into git.

## Next steps

1. Phase 2 without a paybill: slice 4 (reversals, player debt, internal reconciliation; ADR 0007) is done; next
   realtime confirmation (slice 5). Sandbox top-ups are live (2026-10-22): the Daraja sandbox credentials are set on Render and the api
   logs `payments=true`. The owner's test account (0796992457) is KYC-verified with provider
   `owner-attested-2026-10-22` (no registry vendor yet); its phone is proved by its first approved top-up prompt.
2. Owner: run trusted-proxy probe 2 from a second network.
3. Owner: choose the national-registry (IPRS) provider.
4. Later: password reset (STK proof + ID match), passkeys/TOTP, real Play Integrity / App Attest verifiers, nightly
   mutation testing of the integration-backed packages.

## Open questions for the owner

- Confirm the 5% deposit excise version with a tax adviser before production.
- Does GRA accept phone verification by M-Pesa confirmation instead of SMS OTP? (ADR 0005)
- Minimum verification deposit amount (suggested KES 10) and excise treatment on it.
- `internal/notifications` (SMS sender) is unused since ADR 0005: keep for a future channel or delete?
