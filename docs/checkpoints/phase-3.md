# Phase 3 — Game and tickets: checkpoint

Status: IN PROGRESS (started 2026-10-24 on the owner's go-ahead).

## Owner decisions (2026-10-24)
- **Price: KES 100 per play.** Versioned with the odds (`game.odds_config.price_kes`), so a later price applies from a
  play date and past tickets keep theirs.
- **4 plays per player per play day** (already a database constraint). The **play day runs midnight to midnight
  Africa/Nairobi**, the same windows as responsible gaming.
- **Each play is one provably fair spin** over the configured slots (placeholder: 12 slots, 1 lucky). **A lucky spin
  marks the day lucky, which doubles that day's draw chances.** Chances come from the tier table by plays that day
  (placeholder 1→1, 2→2, 3→3, 4→5). No instant cash: every prize goes through the audited draws (phase 4).
- Odds and tier values stay placeholders until the owner sets them.

## Plan (written before coding)

Rules that hold across every slice:
- **One transaction per play:** eligibility (`AssertCanPlay`: KYC verified, phone proved), not restricted (RG), no
  player debt (`wallet.Debt == 0`, carried from phase 2), the day's cap, the stake debit (`ledger.Stake`), the ticket,
  the spin and the day's tally commit together or not at all. Idempotent per `(user, Idempotency-Key)`.
- **The cap lives in the database** (`tickets_seq_range`, unique `(user, play_date, seq)`, `paid_plays <= 4`); the
  service locks the player's day row so concurrent plays queue, and a race test checks exactly 4 of 10 succeed.
- **Provably fair:** each player has an active seed chain: a secret server seed (stored encrypted) whose SHA-256 was
  shown before any spin used it, the player's client seed, and a nonce that increases by one per spin. A spin's slot is
  HMAC-SHA256(server seed, client seed ":" nonce ":" round) read as 32-bit words, with rejection sampling (no modulo
  bias). Rotating the chain reveals the old seed so every past spin can be checked with the public verifier
  (`tools/verifier`), which shares no code path with the server beyond the specification.
- Spins are append-only (trigger, phase 1). The odds version used is recorded on each spin.

### Slice 1 — Fairness engine and verifier
`internal/game/fair` (seed generation, commitment, slot derivation, client-seed rules), `tools/verifier` (command line,
independent re-implementation), unit tests with fixed vectors shared by both, a 100k-spin chi-square test in CI and a
10-million-spin distribution test in the nightly.

### Slice 2 — Play
Migration (price on the odds version), `internal/game` service: seed chains (create on first play, rotate with reveal,
set client seed), `Play` in one transaction, today's state (plays, lucky, chances, price). API `POST /v1/plays`,
`GET /v1/plays/today`, `GET /v1/fairness`, `POST /v1/fairness/rotate`; OpenAPI; auth/IDOR/idempotency sweeps.

### Slice 3 — Checkpoint
RTP/odds simulation in `lotteryctl`, reconciliation of stakes (`STAKES_REVENUE` = sum of paid tickets × price),
mutation testing of `game`, checkpoint report.

## Results

### Slice 1 (fairness engine and verifier)
- `internal/game/fair`: server seed (32 random bytes), commitment (hex SHA-256), client-seed rules
  (`[A-Za-z0-9_-]{1,64}`), slot derivation (HMAC-SHA256 words with rejection sampling), `Verify` (seed against
  commitment, then the slot).
- `tools/verifier`: command-line re-implementation that does not import the server's package; prints each nonce's
  slot and whether it is lucky.
- Test vectors computed by a third implementation (Python, from the written specification), including one where the
  first word is rejected (nonce 456436 over 40000 slots); both Go implementations must reproduce all of them.
- Uniformity: chi-square over 100k spins in CI (p = 0.001 threshold); 10 million spins over 12 and over 7 slots in
  the nightly (`-tags distribution`, about a minute).
