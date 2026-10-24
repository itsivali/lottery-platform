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

-- name: InsertChain :one
INSERT INTO game.seed_chains (user_id, seed_enc, commitment, client_seed, created_at)
VALUES ($1, $2, $3, $4, $5) RETURNING id;

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
