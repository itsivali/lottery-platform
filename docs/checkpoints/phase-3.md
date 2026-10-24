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

### Slice 2 (play)
- Migration `00019`: `price_kes` on the odds version (KES 100), `revealed_seed` on rotated chains (published, checked
  by constraint to be present exactly when rotated).
- `internal/game`: `Play` in one transaction (replay by key, `AssertCanPlay`, RG, no debt, odds for the Nairobi day,
  day row locked, chain locked or created, slot, ticket, `ledger.Stake` through the wallet, spin, nonce, day tally);
  `Today` (plays, plays left, lucky, chances = tier × 2 on a lucky day, price, tickets with their spins); `Fairness`
  (creates the first chain so its commitment is shown before any spin); `Rotate` (reveals the seed after checking it
  against the commitment, starts a new chain with the player's client seed). Server seeds are encrypted with the data
  keyring, bound to the player.
- API: `POST /v1/plays` (201, or 200 for a replayed key), `GET /v1/plays/today`, `GET /v1/fairness`,
  `POST /v1/fairness/rotate`; problems `daily-limit`, `insufficient-funds`, `in-debt`, `restricted`, `kyc-required`,
  `phone-not-verified`, `invalid-client-seed`. OpenAPI updated (and every tag now has a description: no lint warnings).
- Tests (integration): price debited and booked to `STAKES_REVENUE`; Nairobi play date at 23:30; spin verified after
  rotation against the commitment shown before it; the next play uses the new chain from nonce 0; same key charges
  once; 10 concurrent plays → exactly 4, the rest `daily-limit`, consecutive nonces; the cap resets after Nairobi
  midnight; chances follow the tiers and double on a lucky day; refusals (low balance, unproved phone, time-out, debt)
  charge and count nothing; concurrent first fairness requests create one chain. API: the journey to the daily limit,
  error mapping, and the new routes in the auth and idempotency sweeps.

### Slice 3 (reconciliation, simulation, checkpoint)
- Migration `00020`: `game.reconciliation_runs` (append-only).
- `game.Reconcile` (daily River job `game.reconcile`, on start too; `lotteryctl game reconcile`): ticket ⇔ stake journal
  ⇔ price, `STAKES_REVENUE` = paid tickets, ticket ⇔ spin, player day ⇔ tickets and luck, nonces without gaps,
  published seeds ⇔ commitments, and every spin of chains revealed in the last 48 hours recomputed from its seed. One
  test per kind of tampering. Runbook: `docs/runbooks/game-operations.md`.
- `game.Simulate` / `lotteryctl game simulate` (no database): lucky-day rate and draw chances per number of daily
  plays from real fair spins, against the theory; in the nightly on the odds in force.
- Mutation testing of `game` in the nightly, with its integration tests (like `wallet`).

**Simulation on the placeholder odds** (12 slots, 1 lucky, KES 100, tiers 1→1, 2→2, 3→3, 4→5; 50,000 days each):

| Plays a day | Stake | Lucky days (theory) | Mean chances (theory) | KES per chance |
|---|---|---|---|---|
| 1 | 100 | 8.1% (8.3%) | 1.08 (1.08) | 92 |
| 2 | 200 | 16.0% (16.0%) | 2.32 (2.32) | 86 |
| 3 | 300 | 23.0% (23.0%) | 3.69 (3.69) | 81 |
| 4 | 400 | 29.3% (29.4%) | 6.47 (6.47) | 62 |

**For the owner:** with these placeholder tiers a chance costs a third less on the 4th play of the day than on the
1st, a reward for playing the maximum. A regulator (and our own responsible-gaming principles) may see that as an
incentive to play more; flat tiers (1→1 … 4→4) would make every play worth the same. Decide with the real values.
