# Runbook: draws

Daily, weekly, monthly and quarterly draws (phase 4, `docs/checkpoints/phase-4.md`). The worker runs the schedule every
5 minutes; officials sign and approve with `lotteryctl` (back office later).

## Every morning

1. After 00:05 Nairobi the period's draws are **frozen**: `go run ./cmd/lotteryctl draws list`.
2. Two different officials each run, **before 09:00** (the drand round is published then):

   ```sh
   go run ./cmd/lotteryctl draws show -draw <id>        # entries, entry_list_hash, seed_commit, drand_round_at
   go run ./cmd/lotteryctl draws sign -draw <id> -operator <your name>
   ```

   A draw not signed by two officials before its round is voided and re-run automatically with the next 09:00 round.
3. After 09:00 the worker conducts it with the verified drand beacon. Check the results and certificate (`draws show`),
   then two officials approve; approval moves the pool into `PRIZE_POOL_*`:

   ```sh
   go run ./cmd/lotteryctl draws approve -draw <id> -operator <your name>
   ```

## Voiding

Only a frozen or signed draw, and only before its round: `draws void -draw <id> -reason "<why>" -operator <name>`
(needs the api's configuration: the re-run's seed is encrypted with `DATA_KEY`). A conducted draw cannot be voided.

## When drand is down

The draw stays signed and the worker tries every 5 minutes (relays: api, api2, api3.drand.sh, Cloudflare). Beacons are
verified against the pinned quicknet key whichever relay serves them.
