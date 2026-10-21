## What and why

## TDD checklist
- [ ] A failing test was written first and seen failing for the right reason
- [ ] Test and code are in the same commit(s); every commit is green
- [ ] No skipped tests (or skip carries `// ADR-NNNN`)
- [ ] `make ci` passes locally
- [ ] Money paths use whole-KES `bigint`; no floats
- [ ] No secrets or real credentials
- [ ] Docs / ADR / open-decisions register updated if a decision or placeholder was touched
