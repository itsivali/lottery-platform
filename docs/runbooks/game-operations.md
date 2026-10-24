# Runbook: game operations

Plays, tickets and provably fair spins (phase 3). Rules: KES 100 a play (the odds version's price), at most 4 plays
per player per Nairobi day, a lucky spin doubles that day's draw chances.

## Daily reconciliation

Runs in the worker once a day (River job `game.reconcile`, also on start) and on demand:

```sh
go run ./cmd/lotteryctl game reconcile      # needs DATABASE_URL only; exits 1 on a mismatch
```

Every run is stored in `game.reconciliation_runs` (append-only). Mismatches never happen in normal operation: each is a
bug or tampering: tell the owner at once and investigate before anything else.

| Mismatch | Meaning |
|---|---|
| `ticket_without_stake` / `stake_without_ticket` | a ticket and its stake journal are not paired |
| `stake_amount_mismatch` | the stake booked to `STAKES_REVENUE` differs from the ticket's price |
| `revenue_vs_tickets` | `STAKES_REVENUE` differs from the price of all paid tickets |
| `ticket_without_spin` | a ticket has no spin |
| `day_tally_mismatch` | a player's day (`user date`) counts plays or luck its tickets and spins do not show |
| `nonce_gap` | a seed chain's spins are not exactly nonces 0 .. next_nonce-1 |
| `revealed_seed_mismatch` | a published seed does not hash to the commitment shown before its spins |
| `spin_not_reproducible` | a spin of a chain revealed in the last 48 hours does not recompute from its published seed |

## Checking a spin for a player

A player rotates their chain (`POST /v1/fairness/rotate`), which publishes the old server seed. With the commitment
they were shown before playing (`GET /v1/fairness` before the spin, or the `commitment` on the play), the client seed,
the nonce and the odds on the spin:

```sh
go run ./tools/verifier -server-seed SEED -commitment COMMITMENT -client-seed CLIENT -nonce 0-3 -slots 12 -lucky 1
```

The verifier does not use the server's code; it prints `commitment ok` and each nonce's slot, or `MISMATCH`.

## Odds simulation

```sh
go run ./cmd/lotteryctl game simulate [-days 1000000] [-slots 12] [-lucky 1] [-price 100]   # no database needed
```

Prints, for players who play 1 to 4 times a day: how often the day is lucky (measured and in theory), the mean draw
chances and what a chance costs. The nightly runs it on the odds version in force. Return to player needs prize
structures (phase 4).
