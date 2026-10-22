# Status

Read this first at the start of every session.

- **Phase:** 1 — Foundations: complete. **Phase 2 — Payments: in progress** (plan and progress in
  `docs/checkpoints/phase-2.md`). Slice 1 (Daraja client + fault-injecting mock) is on branch `feat/payments-daraja-client`.
- **Repo state:** `main` is green as of PR #24.
- **Deployed (verified 2026-10-22):** Render Blueprint `lottery-platfform` is in sync with `main`. `lottery-api`
  (`https://lottery-api-7jmj.onrender.com`, free, Oregon) is live at `56bcbaf`; `/healthz` and `/readyz` answer 200; `/v1`
  is enabled; migrations at version 14. Free Postgres (`lottery-db`, PG 18) and Key Value (`lottery-cache`) are available.
- **`DATA_KEY` and `IDENTITY_KEY` were handed to the owner for the password manager on 2026-10-22.** `AUTH_KEY` should be
  saved too (copy it from the dashboard).

## Blocking before the API is exposed to players

1. **Trusted-proxy probe 1 FAILED (2026-10-22).** 61 logins with distinct forged `X-Forwarded-For` values all returned
   `401`; none hit the per-IP limit. The rightmost entry is therefore not a stable client address: either Render passes
   the client's header through unchanged (spoofable) or the rightmost entry is a rotating proxy address. Per the runbook,
   do not expose the API until this is fixed. Options: (a) find the header Render/Cloudflare sets that a client cannot
   forge (e.g. `CF-Connecting-IP`, `True-Client-IP`), confirm it by temporarily logging the headers, then set
   `TRUSTED_PROXY_HEADER` to it and re-run both probes; (b) as a stop-gap, `TRUSTED_PROXY_HEADER=none` (all clients share
   one per-IP budget: safe but coarse; the config test currently forbids it in `render.yaml`).
2. **The free Postgres expires on 2026-11-20** (Render deletes free databases after 30 days). Upgrade or re-create it
   before then; anything stored is lost otherwise.
3. Run the two queries in `docs/runbooks/database-roles.md` (single-role mode or not); the deploy logs show no warning,
   but that is not proof.
4. Delete the stale `OTP_KEY` env var on the Render service (unused since ADR 0005).

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
- Daraja: **sandbox only** (shortcode 174379). The owner's sandbox consumer key/secret and passkey exist and work; they go
  into Render as `sync: false` variables when slice 2 wires payments, never into git.

## Next steps

1. Merge slice 1, then slice 2 (STK top-up end to end) per `docs/checkpoints/phase-2.md`.
2. Fix the trusted-proxy header (above) and re-run both probes.
3. Owner: choose the national-registry (IPRS) provider.
4. Later: password reset (STK proof + ID match), passkeys/TOTP, real Play Integrity / App Attest verifiers, nightly
   mutation testing of the integration-backed packages. Docker is needed locally for `make test-int` / `make ci`.

## Open questions for the owner

- Confirm the 5% deposit excise version with a tax adviser before production.
- Does GRA accept phone verification by M-Pesa confirmation instead of SMS OTP? (ADR 0005)
- Minimum verification deposit amount (suggested KES 10) and excise treatment on it.
- `internal/notifications` (SMS sender) is unused since ADR 0005: keep for a future channel or delete?
