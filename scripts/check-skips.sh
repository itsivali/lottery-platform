#!/usr/bin/env bash
# Reject t.Skip / it.skip unless the line carries an ADR reference (// ADR-NNNN).
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
bad=$(grep -rnE --include='*_test.go' --include='*.test.ts' --include='*.test.tsx' --include='*.spec.ts' \
  -e 't\.Skip(Now|f)?\(' -e '\b(it|test|describe)\.skip\(' . 2>/dev/null \
  | grep -vE '(//|#) *ADR-[0-9]{4}' | grep -v 'scripts/check-skips' || true)
if [ -n "$bad" ]; then
  echo "Skipped tests without an ADR reference:" >&2
  echo "$bad" >&2
  exit 1
fi
echo "no unreferenced skipped tests"
