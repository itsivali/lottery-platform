-- name: EnsureOpenDraw :exec
-- the current period's draw, so the schedule can be shown; nothing when a live draw for the period exists
INSERT INTO draws.draws (draw_type, period_start, period_end, created_at, updated_at)
VALUES (sqlc.arg(draw_type), sqlc.arg(period_start), sqlc.arg(period_end), sqlc.arg(at), sqlc.arg(at))
ON CONFLICT (draw_type, period_start) WHERE status <> 'voided' DO NOTHING;

-- name: InsertRerun :one
INSERT INTO draws.draws (draw_type, period_start, period_end, rerun_of, created_at, updated_at)
VALUES (sqlc.arg(draw_type), sqlc.arg(period_start), sqlc.arg(period_end), sqlc.arg(rerun_of), sqlc.arg(at), sqlc.arg(at))
RETURNING id;

-- name: DrawsInStatus :many
SELECT id FROM draws.draws WHERE status = $1 ORDER BY period_end, draw_type;

-- name: GetDraw :one
SELECT id, draw_type, period_start, period_end, status, seed_commit, seed_enc, revealed_seed, beacon_round, beacon_value,
       entry_list_hash, certificate_json, certificate_hash, frozen_at, voided_reason, rerun_of, created_at, updated_at
FROM draws.draws WHERE id = $1;

-- name: LockDraw :one
SELECT id, draw_type, period_start, period_end, status, seed_commit, seed_enc, revealed_seed, beacon_round, beacon_value,
       entry_list_hash, certificate_json, certificate_hash, frozen_at, voided_reason, rerun_of, created_at, updated_at
FROM draws.draws WHERE id = $1 FOR UPDATE;

-- name: ListDraws :many
SELECT id FROM draws.draws ORDER BY period_end DESC, draw_type, created_at DESC LIMIT sqlc.arg(max_rows);

-- name: PlayerDaysIn :many
-- every player day with plays in the period, with the ticket of its last play, in entry order
SELECT d.user_id, d.play_date, d.paid_plays, d.lucky,
       (SELECT t.id FROM tickets.tickets t WHERE t.user_id = d.user_id AND t.play_date = d.play_date ORDER BY t.seq DESC LIMIT 1)::uuid AS ticket_id
FROM tickets.player_days d
WHERE d.play_date BETWEEN sqlc.arg(period_start) AND sqlc.arg(period_end) AND d.paid_plays > 0
ORDER BY d.play_date, d.user_id;

-- name: TiersOn :many
SELECT t.plays, t.chances FROM game.chance_tiers t
WHERE t.effective_from = (SELECT max(v.effective_from) FROM game.chance_tiers v WHERE v.effective_from <= sqlc.arg(play_date)::date)
ORDER BY t.plays;

-- name: InsertEntries :copyfrom
INSERT INTO draws.draw_entries (draw_id, entry_no, user_id, play_date, ticket_id) VALUES ($1, $2, $3, $4, $5);

-- name: Entries :many
SELECT id, entry_no, user_id, play_date, ticket_id FROM draws.draw_entries WHERE draw_id = $1 ORDER BY entry_no;

-- name: Freeze :exec
UPDATE draws.draws
   SET status = 'frozen', entry_list_hash = sqlc.arg(entry_list_hash), seed_commit = sqlc.arg(seed_commit),
       seed_enc = sqlc.arg(seed_enc), beacon_round = sqlc.arg(beacon_round), frozen_at = sqlc.arg(at), updated_at = sqlc.arg(at)
 WHERE id = sqlc.arg(id) AND status = 'open';

-- name: SetStatus :exec
UPDATE draws.draws SET status = sqlc.arg(status), updated_at = sqlc.arg(at) WHERE id = sqlc.arg(id);

-- name: VoidDraw :exec
UPDATE draws.draws SET status = 'voided', voided_reason = sqlc.arg(reason), updated_at = sqlc.arg(at) WHERE id = sqlc.arg(id);

-- name: Conduct :exec
UPDATE draws.draws
   SET status = 'conducted', revealed_seed = sqlc.arg(revealed_seed), beacon_value = sqlc.arg(beacon_value),
       certificate_json = sqlc.arg(certificate_json), certificate_hash = sqlc.arg(certificate_hash), updated_at = sqlc.arg(at)
 WHERE id = sqlc.arg(id) AND status = 'signed';

-- name: InsertSignoff :exec
INSERT INTO draws.draw_signoffs (draw_id, step, organisation, official, signature, signed_at)
VALUES ($1, $2, 'company', $3, $4, $5);

-- name: SignoffCount :one
SELECT count(DISTINCT official)::int FROM draws.draw_signoffs WHERE draw_id = $1 AND step = $2;

-- name: Signoffs :many
SELECT step, organisation, official, signature, signed_at FROM draws.draw_signoffs WHERE draw_id = $1 ORDER BY signed_at;

-- name: PrizeStructureFor :one
SELECT id, kind, rounding, coalesce(pool_share::text, '0')::text AS pool_share, prizes FROM draws.prize_structures
WHERE draw_type = sqlc.arg(draw_type) AND effective_from <= sqlc.arg(on_date)::date
ORDER BY effective_from DESC LIMIT 1;

-- name: StakesIn :one
SELECT coalesce(sum(price_kes), 0)::bigint FROM tickets.tickets
WHERE status = 'paid' AND play_date BETWEEN sqlc.arg(period_start) AND sqlc.arg(period_end);

-- name: InsertResult :exec
INSERT INTO draws.draw_results (draw_id, entry_id, prize_rank, amount_kes, created_at) VALUES ($1, $2, $3, $4, $5);

-- name: Results :many
SELECT r.prize_rank, r.amount_kes, e.entry_no, e.user_id, e.id AS entry_id
FROM draws.draw_results r JOIN draws.draw_entries e ON e.id = r.entry_id
WHERE r.draw_id = $1 ORDER BY r.prize_rank;
