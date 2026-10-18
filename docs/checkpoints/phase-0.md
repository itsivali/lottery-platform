# Phase 0 — Bootstrap: checkpoint

Status: IN PROGRESS

## Plan (written before coding)

Order of work:
1. Docs: BUILD_PROMPT.md, PROJECT_DESCRIPTION.pdf, OPEN_DECISIONS.md, ADR template + ADR 0001/0002, ci-secrets.md.
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
(filled in at the end of the phase)
