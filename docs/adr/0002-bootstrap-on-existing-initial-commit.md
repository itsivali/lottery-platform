# 0002. Bootstrap on top of the existing initial commit

Status: accepted
Date: 2026-10-18

## Context
The build brief assumed an empty repository and a single initial commit. The remote already contained `ad3007d Initial commit` (README only).

## Decision
Add the bootstrap on top of `ad3007d`. No force-push, no history rewrite.

## Consequences
`main` history starts with two commits. All commits are authored as Willis Ivali <itsivali@outlook.com>.
