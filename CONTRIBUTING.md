# Contributing

## Test-driven development is mandatory
- Red → green → refactor, one behaviour at a time. Write a test that fails for the right reason, run it, then write the minimum code to pass.
- Name tests after the rule: `TestPlay_FifthPlayOfPlayDayIsRefused`, `TestLedger_EveryJournalBalances`.
- Every commit is green and contains the test together with the code that makes it pass. Never commit a failing or skipped test. `t.Skip` / `it.skip` are rejected by CI unless the line carries an `// ADR-NNNN` reference.
- Bug fixes start with a failing test that reproduces the bug.
- Go tests run with `-race -shuffle=on -count=1`; inject the clock and RNG; no sleeps (use polling helpers).
- Coverage gates: 85% on wallet, ledger, tickets, game, draws, payments, payouts; 70% overall.

## Workflow
1. `mise install && make setup`
2. Branch from `main`: `feat/…`, `fix/…`, `chore/…`.
3. `make ci` before pushing. Hooks (lefthook) help, but CI and branch protection are the gate.
4. Open a PR; squash-merge when `ci-ok` is green.
5. Conventional Commits (`feat(wallet): …`). Commits are authored under the committer's real name.

## Rules
- Money is whole KES `bigint` (ADR 0001). No floats in the money path.
- No secrets in the repo. No real Daraja production credentials anywhere.
- Non-obvious technical choices get an ADR in `docs/adr/`. Placeholders for business values go in `docs/OPEN_DECISIONS.md`.
- Stub security closed (refuse), never open.
