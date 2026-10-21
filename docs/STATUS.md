# Status

Read this first at the start of every session.

- **Phase:** 1 — Foundations: COMPLETE, awaiting human go-ahead (see `docs/checkpoints/phase-1.md`).
- **Last completed step:** PRs #12-#17 merged; observability, ERD and the Phase 1 checkpoint are in the final Phase 1 PR.
- **Next step:** Phase 2 — Payments (Daraja client and mock with fault injection, STK top-up, C2B, verification via
  status query, reconciliation, suspense, reversals, excise posting, WebSocket confirmation). Do not start until a human replies.
- **Open questions for the human:** (1) wire `/v1` into `cmd/api` now or in phase 2 (needs secrets, Valkey, SMS adapter);
  (2) confirm Render's database user can own objects as `app_migrator`; (3) the TDD-order and flake notes in the checkpoint.
