#!/usr/bin/env bash
# Coverage gate: 85% on money-path packages (once they have code), 70% overall.
set -euo pipefail
prof=${1:-coverage.out}
mod=github.com/itsivali/lottery-platform
fail=0
# Entry points (main.go), local mock servers and the testcontainers helper are not unit-testable
# business code; they are exercised by the compose e2e smoke test instead.
filtered=$(mktemp)
trap 'rm -f "$filtered"' EXIT
grep -vE '/main\.go:|/mocks/|/internal/platform/test(pg|valkey)/|/store/(db|models|[a-z_]+\.sql)\.go:' "$prof" > "$filtered"
prof="$filtered"
total=$(go tool cover -func="$prof" | awk '/^total:/ {gsub("%","",$3); print $3}')
echo "overall coverage: ${total}% (gate 70%)"
awk -v t="$total" 'BEGIN{exit !(t+0 < 70)}' && { echo "FAIL: overall below 70%"; fail=1; }
for pkg in wallet ledger tickets game draws payments payouts; do
  path="$mod/internal/$pkg/"
  # only gate packages that have non-doc statements
  lines=$(grep -c "^$path" "$prof" || true)
  [ "$lines" -eq 0 ] && { echo "internal/$pkg: no code yet (gate skipped)"; continue; }
  # a block appears once per test binary under -coverpkg: it is covered if any copy ran
  pct=$(grep "^$path" "$prof" | awk '{k=$1; n[k]=$2; if ($3>0) hit[k]=1} END{for (k in n){tot+=n[k]; if (k in hit) cov+=n[k]} printf "%.1f", tot?100*cov/tot:0}')
  echo "internal/$pkg: ${pct}% (gate 85%)"
  awk -v t="$pct" 'BEGIN{exit !(t+0 < 85)}' && { echo "FAIL: internal/$pkg below 85%"; fail=1; }
done
exit $fail
