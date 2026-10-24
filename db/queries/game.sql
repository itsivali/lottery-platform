-- name: OddsFor :one
-- The odds version in force on a play date.
SELECT id, slot_count, lucky_slots, price_kes FROM game.odds_config
WHERE effective_from <= sqlc.arg(play_date)::date ORDER BY effective_from DESC LIMIT 1;

-- name: TiersFor :many
-- The chance tiers in force on a play date.
SELECT t.plays, t.chances FROM game.chance_tiers t
WHERE t.effective_from = (SELECT max(v.effective_from) FROM game.chance_tiers v WHERE v.effective_from <= sqlc.arg(play_date)::date)
ORDER BY t.plays;

-- name: EnsurePlayerDay :exec
INSERT INTO tickets.player_days (user_id, play_date, updated_at) VALUES ($1, $2, $3)
ON CONFLICT (user_id, play_date) DO NOTHING;

-- name: LockPlayerDay :one
SELECT paid_plays, lucky FROM tickets.player_days WHERE user_id = $1 AND play_date = $2 FOR UPDATE;

-- name: GetPlayerDay :one
SELECT paid_plays, lucky FROM tickets.player_days WHERE user_id = $1 AND play_date = $2;

-- name: RecordPlay :exec
UPDATE tickets.player_days
   SET paid_plays = paid_plays + 1, lucky = lucky OR sqlc.arg(lucky), updated_at = sqlc.arg(at)
 WHERE user_id = sqlc.arg(user_id) AND play_date = sqlc.arg(play_date);

-- name: LockActiveChain :one
SELECT id, seed_enc, commitment, client_seed, next_nonce, created_at FROM game.seed_chains
WHERE user_id = $1 AND status = 'active' FOR UPDATE;

-- name: GetActiveChain :one
SELECT id, commitment, client_seed, next_nonce, created_at FROM game.seed_chains WHERE user_id = $1 AND status = 'active';

-- name: InsertChain :exec
-- does nothing when the player already has an active chain (a concurrent request created it first)
INSERT INTO game.seed_chains (user_id, seed_enc, commitment, client_seed, created_at)
VALUES ($1, $2, $3, $4, $5) ON CONFLICT (user_id) WHERE status = 'active' DO NOTHING;

-- name: AdvanceNonce :exec
UPDATE game.seed_chains SET next_nonce = next_nonce + 1 WHERE id = $1;

-- name: RotateChain :exec
UPDATE game.seed_chains SET status = 'rotated', revealed_at = sqlc.arg(at), revealed_seed = sqlc.arg(revealed_seed)
WHERE id = sqlc.arg(id) AND status = 'active';

-- name: RotatedChains :many
SELECT id, commitment, client_seed, next_nonce, revealed_seed, created_at, revealed_at FROM game.seed_chains
WHERE user_id = $1 AND status = 'rotated' ORDER BY created_at DESC LIMIT 20;

-- name: TicketByKey :one
SELECT id FROM tickets.tickets WHERE user_id = $1 AND idempotency_key = $2;

-- name: InsertTicket :one
INSERT INTO tickets.tickets (user_id, play_date, seq, price_kes, idempotency_key, created_at)
VALUES ($1, $2, $3, $4, $5, $6) RETURNING id;

-- name: InsertSpin :exec
INSERT INTO game.spins (ticket_id, chain_id, client_seed, nonce, slot, lucky, odds_config_id, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: GetPlay :one
SELECT t.id, t.user_id, t.play_date, t.seq, t.price_kes, t.status, t.created_at,
       s.chain_id, s.client_seed, s.nonce, s.slot, s.lucky, c.commitment, o.slot_count, o.lucky_slots
FROM tickets.tickets t
JOIN game.spins s ON s.ticket_id = t.id
JOIN game.seed_chains c ON c.id = s.chain_id
JOIN game.odds_config o ON o.id = s.odds_config_id
WHERE t.id = $1;

-- name: PlaysOn :many
SELECT t.id FROM tickets.tickets t WHERE t.user_id = $1 AND t.play_date = $2 ORDER BY t.seq;

-- Reconciliation. Each "mismatch" query returns the ids that break an invariant; each must return nothing.

-- name: TicketsWithoutStake :many
SELECT t.id FROM tickets.tickets t
WHERE NOT EXISTS (SELECT 1 FROM ledger.ledger_journals j WHERE j.kind = 'stake' AND j.reference_type = 'ticket' AND j.reference_id = t.id)
ORDER BY t.id;

-- name: StakesWithoutTicket :many
SELECT j.reference_id::text FROM ledger.ledger_journals j
WHERE j.kind = 'stake' AND j.reference_type = 'ticket'
  AND NOT EXISTS (SELECT 1 FROM tickets.tickets t WHERE t.id = j.reference_id)
ORDER BY 1;

-- name: StakeAmountMismatch :many
-- the stake credited to STAKES_REVENUE is the ticket's price
SELECT t.id FROM tickets.tickets t
JOIN ledger.ledger_journals j ON j.kind = 'stake' AND j.reference_type = 'ticket' AND j.reference_id = t.id
JOIN ledger.ledger_entries e ON e.journal_id = j.id AND e.direction = 'C'
JOIN ledger.ledger_accounts a ON a.id = e.account_id AND a.code = 'STAKES_REVENUE'
GROUP BY t.id, t.price_kes HAVING sum(e.amount_kes) <> t.price_kes
ORDER BY t.id;

-- name: TicketsWithoutSpin :many
SELECT t.id FROM tickets.tickets t WHERE NOT EXISTS (SELECT 1 FROM game.spins s WHERE s.ticket_id = t.id) ORDER BY t.id;

-- name: DayTallyMismatch :many
-- a player's day counts exactly the tickets sold that day, and is lucky exactly when one of their spins was
SELECT (d.user_id::text || ' ' || d.play_date::text)::text AS day FROM tickets.player_days d
LEFT JOIN (SELECT t.user_id, t.play_date, count(*) AS n, bool_or(s.lucky) AS lucky
           FROM tickets.tickets t JOIN game.spins s ON s.ticket_id = t.id GROUP BY t.user_id, t.play_date) x
  ON x.user_id = d.user_id AND x.play_date = d.play_date
WHERE d.paid_plays <> coalesce(x.n, 0) OR d.lucky <> coalesce(x.lucky, false)
ORDER BY 1;

-- name: NonceGaps :many
-- a chain's spins use nonces 0 .. next_nonce-1, each once
SELECT c.id FROM game.seed_chains c
LEFT JOIN (SELECT chain_id, count(*) AS n, max(nonce) AS hi FROM game.spins GROUP BY chain_id) s ON s.chain_id = c.id
WHERE coalesce(s.n, 0) <> c.next_nonce OR (s.n > 0 AND s.hi <> c.next_nonce - 1)
ORDER BY c.id;

-- name: RevealedSeedMismatch :many
SELECT id FROM game.seed_chains
WHERE status = 'rotated' AND encode(sha256(decode(revealed_seed, 'hex')), 'hex') <> commitment
ORDER BY id;

-- name: RevealedSpins :many
-- spins of chains revealed since a time, with what is needed to recompute them
SELECT s.ticket_id, s.client_seed, s.nonce, s.slot, s.lucky, c.revealed_seed, o.slot_count, o.lucky_slots
FROM game.spins s
JOIN game.seed_chains c ON c.id = s.chain_id
JOIN game.odds_config o ON o.id = s.odds_config_id
WHERE c.status = 'rotated' AND c.revealed_at >= sqlc.arg(since)::timestamptz
ORDER BY s.ticket_id;

-- name: StakesRevenue :one
-- credit-normal balance of STAKES_REVENUE
SELECT coalesce(sum(CASE e.direction WHEN 'C' THEN e.amount_kes ELSE -e.amount_kes END), 0)::bigint
FROM ledger.ledger_entries e JOIN ledger.ledger_accounts a ON a.id = e.account_id WHERE a.code = 'STAKES_REVENUE';

-- name: PaidTicketsTotal :one
SELECT count(*)::bigint AS tickets, coalesce(sum(price_kes), 0)::bigint AS kes FROM tickets.tickets WHERE status = 'paid';

-- name: InsertReconciliationRun :exec
INSERT INTO game.reconciliation_runs (ran_at, ok, report) VALUES ($1, $2, $3);
