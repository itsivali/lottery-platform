# Phase 4 — Draws: checkpoint

Status: IN PROGRESS (started 2026-10-24 on the owner's go-ahead).

## Owner decisions (2026-10-24)
- **Draws:** daily, weekly, monthly and quarterly. **A play day's chances enter all four**: that day's daily draw and
  the week's (Monday–Sunday), month's and quarter's draws that contain the day. One entry per chance.
- **Prizes are a share of the period's stakes** (`pool_share`): each draw's pool is a percentage of the stakes sold in
  its period, split across ranks. The operator never owes more than it took in. **Values are placeholders** until the
  owner sets them: daily 30%, weekly 10%, monthly 5%, quarterly 5% of stakes (50% in all); ranks 50/30/20% of the pool.
- **Timing:** entries freeze at **00:05 Nairobi** after the period ends; the draw uses the first drand round published
  at or after **09:00 Nairobi** that day (owner, 2026-10-24: changed from 00:30 so the officials can sign in working
  hours). Daily for the previous play day; weekly on Monday, monthly on the 1st, quarterly on the 1st of
  January/April/July/October, for the period just ended.
- **Sign-off:** two different company officials sign the frozen entry list (`signed`) and approve the result
  (`approved`), through `lotteryctl` until the back office (phase 6). The GRA official's step is added when GRA names one.

## Plan (written before coding)

Rules across every slice:
- **Explicit state machine** (`open → frozen → signed → conducted → approved → paid`, or `voided` and re-run as a new
  row). Every transition is one transaction that checks the current state; the one-time seed and the append-only
  entries and results are already database rules (phase 1).
- **Nobody can know the outcome before it is fixed.** At freeze the entry list is numbered and hashed, the company
  commits to a fresh secret seed (`seed_commit`), and the drand round is fixed: the first round published after the
  signing window closes. Winners come from SHA-256(drand randomness ‖ revealed company seed ‖ entry-list hash ‖ draw id),
  drawn with the same rejection sampling as spins. The company cannot predict drand; drand cannot know the entries or
  the company seed; changing the entries changes the hash both officials signed.
- **The signatures must be complete before the drand round is published.** Signing after it would let a signer see the
  result and void a draw they dislike. A draw not signed by both officials before its round (09:00) is voided and
  re-run with a new round.
- Beacons are verified (BLS signature against the pinned chain key, randomness = SHA-256 of the signature), never just
  fetched.
- Prize amounts: pool = share × stakes of the period (whole KES, the prize structure's rounding), rank amounts =
  pool × rank share; what rounding leaves stays in the pool account. Funding is a ledger posting from
  `STAKES_REVENUE` to the draw type's `PRIZE_POOL_*` at approval; payouts (B2C, withholding tax) are phase 5.
- **One prize per player per draw** (owner, 2026-10-24). Winners are drawn entry by entry from the random stream; an
  entry of a player who already won is skipped, so more entries still mean better odds of winning once. With fewer
  players than ranks, the remaining ranks are not awarded and their share stays in the pool.

### Slice 1 — drand client
`internal/draws/drand`: pinned quicknet chain (hash, public key, genesis, period), round ↔ time, BLS verification
(kyber, `bls-unchained-g1-rfc9380`), fetch through several relays with fallback, every beacon verified whichever relay
served it. Tests on real recorded beacons and tampered ones.

### Slice 2 — Draw lifecycle
Migration (prize-structure placeholders, signing window), `internal/draws` service: open (scheduled), freeze (entries
from tickets and spins, hash, seed commitment, drand round), sign (two different officials, before the round),
conduct (verified beacon, winners), approve (two officials; pool funding posting), void + re-run. River jobs for the
schedule; `lotteryctl draws sign|approve|void|show`. Integration tests for every transition and refusal.

### Slice 3 — Public results and checkpoint
`GET /v1/draws` (schedule, results with drand round, beacon, entry-list hash, revealed seed), `GET /v1/draws/{id}/entries/mine`,
a public draw verifier in `tools/verifier`, reconciliation of pools, monitoring of missed windows, checkpoint report.

## Results

### Slice 1 (drand client)
- `internal/draws/drand`: quicknet pinned (chain hash, G2 public key, genesis, 3 s period), `RoundAt`/`TimeOf`,
  `Verify` (BLS on G1 over SHA-256 of the round, randomness = SHA-256 of the signature), `Fetch` through several relays
  (api, api2, api3, Cloudflare), refusing before the round's time and refusing any beacon that does not verify.
- Tests on three real recorded beacons; refused: another round's signature, a relabelled round, swapped randomness,
  a wrong key, bad hex; a lying relay is skipped for an honest one; outages and not-yet-published rounds. 93.4%.
- kyber's `sign/bls` is marked deprecated for its rogue-key weakness when aggregating signatures under untrusted keys;
  drand verification checks one signature under one pinned group key, which that does not affect.
