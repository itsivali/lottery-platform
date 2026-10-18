# 0001. Money is whole KES stored as bigint

Status: accepted
Date: 2026-10-18

## Context
Invariant 2 requires integer money with no floats. M-Pesa transacts in whole shillings.

## Decision
All amounts are whole Kenyan shillings in `bigint` columns named `*_kes` and `int64` in Go. Tax and excise results are rounded by a single documented rule (half-up, in `internal/ledger`) to whole KES; rates are `numeric(6,5)` in the database and never converted to float.

## Consequences
No cents; sub-shilling amounts are not representable. Rounding residue must be posted explicitly so journals still balance.
