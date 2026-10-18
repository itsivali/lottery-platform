#!/usr/bin/env bash
# Conventional Commits check for the first line of the commit message.
set -euo pipefail
first=$(head -n1 "$1")
re='^(feat|fix|docs|style|refactor|perf|test|build|ci|chore|revert)(\([a-z0-9._/-]+\))?!?: .{1,100}$'
if [[ "$first" =~ ^(Merge|Revert|fixup!|squash!) ]] || [[ "$first" =~ $re ]]; then exit 0; fi
echo "Commit message must follow Conventional Commits: type(scope): subject" >&2
exit 1
