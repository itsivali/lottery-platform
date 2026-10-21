# Status

Read this first at the start of every session.

- **Phase:** 1 — Foundations (in progress)
- **Last completed step:** Slice 1 (schema, merged #12); slice 2 ledger+wallet (PR #13); slice 3 idempotency, rate limit, audit (branch `feat/phase1-idempotency-ratelimit-audit`).
- **Next step:** Slice 4 — identity (OTP, sessions, refresh rotation, SSO linking), KYC mock adapter, RG limits, auth/IDOR tests.
- **Open questions:** none blocking.

Slices for Phase 1 (each is its own PR, squash-merged on green CI):
1. Schema, roles/grants, append-only + balance triggers, clock abstraction
2. Ledger posting rules + wallet core (+ rapid property test, negative-balance concurrency)
3. Idempotency middleware, Valkey rate limiting, audit log service
4. Identity (OTP, sessions, refresh rotation, SSO linking), KYC mock adapter, RG limits, IDOR/auth tests
5. OTel wiring, ERD (`docs/erd.svg`), Phase 1 checkpoint
