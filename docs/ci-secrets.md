# CI secrets and variables

No real values are created by the agent. Add with `gh secret set NAME [--env ENV]`.

| Name | Scope | Used by | Notes |
|---|---|---|---|
| `DARAJA_SANDBOX_CONSUMER_KEY` | env `preview` | preview-e2e.yml | Sandbox only |
| `DARAJA_SANDBOX_CONSUMER_SECRET` | env `preview` | preview-e2e.yml | Sandbox only |
| `DARAJA_SANDBOX_SHORTCODE` | env `preview` | preview-e2e.yml | Sandbox only |
| `DARAJA_SANDBOX_PASSKEY` | env `preview` | preview-e2e.yml | Sandbox only |
| `RENDER_API_KEY` | env `production` | deploy-prod.yml | Scoped key |
| `RENDER_API_SERVICE_IDS` | env `production` | deploy-prod.yml | JSON map service name → id of the Blueprint services, e.g. `{"api":"srv-…"}`; each gets a deploy of the tagged commit |
| `RELEASE_PLEASE_TOKEN` | repo (optional) | release.yml | PAT so release PRs trigger CI; falls back to `GITHUB_TOKEN` |

Keyless cosign and ghcr pushes use the workflow `GITHUB_TOKEN` / OIDC; no secret needed.
