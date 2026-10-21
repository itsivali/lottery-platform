# Phase 0 — Bootstrap: checkpoint

Status: COMPLETE — awaiting human go-ahead

## Plan (written before coding)

Order of work:
1. Docs: build brief, project description and open-decisions register (kept outside the repository), ADR template + ADR 0001/0002, ci-secrets.md.
2. Tooling: `mise.toml` pins; `mise install`.
3. Repo hygiene: README, CONTRIBUTING, SECURITY, CODEOWNERS, editorconfig, gitignore, gitattributes, PR/issue templates, licence notice, dependabot.
4. Layout from brief section 4; `go.mod`; OpenAPI skeleton; goose init migration.
5. Walking skeleton, test-first: `/healthz` 200; `/readyz` 503 when Postgres down, 200 when up; migration runner up→down→up. Then the minimal `api`, `worker`, `lotteryctl migrate`, Dockerfile, Compose stack.
6. Makefile, lefthook, golangci config, skip-check and coverage-gate scripts.
7. Workflows: ci, codeql, images, preview-e2e, release, deploy-prod, nightly, mobile.
8. Local verification, push, `gh run watch`.
9. Branch protection and repo settings via `gh api`.
10. Failing-PR demonstration; this report filled in.

Modules/files touched: `internal/platform/{config,db,clock,httpx}`, `cmd/{api,worker,lotteryctl}`, `mocks/*`, `db/migrations`, `api/openapi.yaml`.
Migrations: `00001_init.sql` (extensions only; real schema is Phase 1).

## Results

Verification (all commands were run; outputs summarised):
- `make ci` locally: lint 0 issues; unit + integration (testcontainers Postgres) pass with `-race -shuffle=on -count=1`; coverage 94.6% (gate 70%; money-path 85% gate has no code yet).
- `docker compose up -d --build --wait`: Postgres, Valkey, Keycloak, Daraja/KYC/SMS/drand mocks, migrate and api all up; `/readyz` 503 with Postgres stopped, 200 after restart. `make e2e` prints `e2e smoke ok`.
- TDD evidence: httpx, config, db/migrations and lotteryctl tests were written and seen failing (package missing / `undefined: parse`) before the code. The real-Postgres `/readyz` test passed on first run (regression coverage for behaviour already implemented).
- Hooks (lefthook) ran on every commit; the pre-push hook blocked a push when Docker was inaccessible.

Green CI runs on main (`gh run list`):
- ci (ci-ok green): https://github.com/itsivali/lottery-platform/actions/runs/39155808054
- ci on 0a46b5d: https://github.com/itsivali/lottery-platform/actions/runs/39156219320
- images (Trivy, SBOM, cosign keyless, ghcr): https://github.com/itsivali/lottery-platform/actions/runs/39156219412
- release (release-please): https://github.com/itsivali/lottery-platform/actions/runs/39156219464

Blocked failing PR: https://github.com/itsivali/lottery-platform/pull/6 — go-unit, go-integration, coverage and ci-ok failed; `gh pr merge` replied "the base branch policy prohibits the merge"; closed unmerged. A direct push to main is rejected (GH006).

Repository settings applied: branch protection on main (PR required, `ci-ok` required + up to date, linear history, no force-push/deletion, admins included, 0 approvals); squash-only merges; delete branch on merge; secret scanning + push protection; Dependabot alerts and security updates; private vulnerability reporting; environments `preview` and `production` (required reviewer itsivali, tags `v*.*.*` only).

Deviations and notes:
- Repo was private on a free plan, which blocks branch protection and code scanning; repo was made public by the owner (not by the agent).
- Bootstrap took 4 commits on main instead of 1: the first CI run was red (golangci config key, Trivy action version, app path filter) and was fixed forward before protection existed; also built on top of the existing initial commit (ADR 0002).
- The red demo commit bypassed local hooks (`LEFTHOOK=0`, `--no-verify`) deliberately so CI was the gate.
- CodeQL TypeScript leg runs only once an `apps/*/package.json` exists.

Deferred: web/back-office/Flutter CI jobs are path-filtered no-ops until Phase 6/7; preview-e2e, deploy-prod and cosign verification at deploy are untested until the secrets in `docs/ci-secrets.md` exist; real Daraja/KYC/SMS/drand mocks arrive in Phases 2-4; nightly mutation/10M-spin jobs have no targets yet.

DECISION items touched: licence (All rights reserved); all others only listed in the open-decisions register.
