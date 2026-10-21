# Status

Read this first at the start of every session.

- **Phase:** 1 — Foundations: complete and merged. Phase 2 (Payments) has **not** been started; wait for the owner's go-ahead.
- **Repo state:** `main` is green (CI `ci-ok`, local `make ci`: lint clean, 88% coverage). PRs #12-#23 merged. History was rewritten
  once to remove the private documents; branch protection on `main` is back to its saved settings.
- **Deployed:** nothing is verified in production yet. The owner was creating a new Render Blueprint (`render.yaml`).

## Decisions taken in the last session (see ADR 0005)

- Accounts are created with **phone number + password** (argon2id, peppered). **No SMS, no OTP, no TOTP for now.**
- Phone ownership is proved by the **M-Pesa STK Push confirmation at the first top-up** (Phase 2 calls `identity.Service.MarkPhoneVerified`).
  A number is unique only once verified. Top-up needs verified KYC; play also needs a proved phone.
- A WhatsApp code was considered and **deferred** (Meta charges per message; the owner wants no recurring costs). It can be added
  later as another caller of `MarkPhoneVerified`.
- KYC runs as a River job. The registry vendor is **not chosen**, Google/Apple developer accounts do not exist yet; both fail closed.
- Free-tier deployment: migrate on start (advisory lock), workers in-process, free Postgres + Key Value.
- The build brief, project description and open-decisions register are **kept outside git** (gitignored in `docs/`).

## Next steps

1. Owner: deploy the Blueprint, then run both probes in `docs/runbooks/trusted-proxy.md` **before** exposing the API, and check
   `docs/runbooks/database-roles.md` (single-role mode or not). Save `DATA_KEY` and `IDENTITY_KEY` somewhere safe (`docs/runbooks/secrets-and-keys.md`).
2. Owner: choose the national-registry (IPRS) provider; create Daraja sandbox credentials (never production ones).
3. Phase 2 — Payments: Daraja client + fault-injecting mock, STK top-up (also the phone proof), C2B, verification via status query,
   reconciliation, suspense, reversals, excise posting, WebSocket confirmation.
4. Password reset (needs the STK proof + ID match), passkeys/TOTP, real Play Integrity / App Attest verifiers, mutation testing of
   the integration-backed packages (nightly), GitHub Support request to purge old commits (`refs/pull/*`) if wanted.

## Open questions for the owner

- Does GRA accept phone verification by M-Pesa confirmation instead of SMS OTP? (ADR 0005)
- Minimum verification deposit amount (suggested KES 10) and excise treatment on it.
- Delete the pre-rewrite backup bundle in the session scratchpad (it still contains the removed documents)?
