#!/usr/bin/env bash
# Phase 0 E2E: bring up the stack and check the probes. Playwright journeys arrive with the clients.
set -euo pipefail
docker compose up -d --build --wait
trap 'docker compose down -v' EXIT
curl -fsS localhost:8080/healthz >/dev/null
curl -fsS localhost:8080/readyz >/dev/null
for p in 8090 8091 8092 8093; do curl -fsS "localhost:$p/healthz" >/dev/null; done
echo "e2e smoke ok"
