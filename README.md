# lottery-platform

Real-money, mobile-money lottery platform for the Kenyan market: Go modular monolith, Postgres, Valkey, M-Pesa (Daraja), provably fair draws, double-entry ledger.

- Build brief (source of truth for the build): [docs/BUILD_PROMPT.md](docs/BUILD_PROMPT.md)
- Project description: [docs/PROJECT_DESCRIPTION.pdf](docs/PROJECT_DESCRIPTION.pdf)
- Open decisions: [docs/OPEN_DECISIONS.md](docs/OPEN_DECISIONS.md)
- Phase checkpoints: [docs/checkpoints/](docs/checkpoints/)

## Setup

Requirements: [mise](https://mise.jdx.dev) and Docker. Nothing else.

```sh
mise install     # pinned Go, Node, Flutter, sqlc, goose, golangci-lint, ...
make setup       # git hooks (lefthook)
make dev         # Postgres, Valkey, Keycloak, mocks, api
make ci          # exactly what CI runs
```

See [CONTRIBUTING.md](CONTRIBUTING.md). Licence: all rights reserved (see [LICENSE](LICENSE)).
