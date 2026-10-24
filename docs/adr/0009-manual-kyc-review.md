# 0009. Identities are reviewed by hand, without a national-registry provider

Status: accepted
Date: 2026-10-24

## Context
KYC was built around a registry gateway (IPRS or a reseller): a River job sends the submission, an exact match
verifies the account, a mismatch goes to manual review. No provider was ever chosen, so every submission waited in
`pending` while the job retried a provider that does not exist, and was then abandoned. The owner decided (2026-10-24)
not to pay for a registry: identities are checked by staff.

## Decision
- With no registry configured (the default, `kyc.ManualProvider`), a submission goes straight to `manual_review`: no
  job, no provider call. The checks that need no registry still run at submission: ID and name format, date of birth,
  **under-18s refused**, one national ID per verified account.
- Staff list waiting submissions with `lotteryctl kyc pending -operator NAME` (the details are decrypted; every one
  shown is audited as `kyc.unmask`) and decide with `lotteryctl kyc approve|reject -user ID -reason TEXT -operator NAME`
  (the existing audited `Review`). Approval claims the national ID; a second account with the same ID cannot be approved.
- The back office (phase 6) replaces the CLI with a review screen. The registry gateway code stays, unused, in case a
  provider is chosen later (`KYC_API_URL`); the compose stack still uses its mock so that path stays tested.
- `KYC_API_URL` / `KYC_API_KEY` leave the Render Blueprint.

## Consequences
- A player waits for a person before they can top up; the review queue needs an owner-defined service level.
- How a reviewer confirms an identity (document photo, call, eCitizen lookup) is the operator's procedure
  (`docs/runbooks/kyc-review.md`); the system records who decided, when and why.
- Regulator acceptance of manual identity checks belongs with the other BCLB/GRA questions (owner).
