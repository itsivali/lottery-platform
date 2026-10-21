-- name: CancelPendingLimits :exec
UPDATE rg.limits SET cancelled_at = sqlc.arg(at)
WHERE user_id = sqlc.arg(user_id) AND period = sqlc.arg(period) AND effective_from > sqlc.arg(at) AND cancelled_at IS NULL;

-- name: UpsertLimit :exec
INSERT INTO rg.limits (user_id, period, amount_kes, effective_from, requested_at)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (user_id, period, effective_from) DO UPDATE
   SET amount_kes = EXCLUDED.amount_kes, requested_at = EXCLUDED.requested_at, cancelled_at = NULL;

-- name: ListLimits :many
SELECT period, amount_kes, effective_from FROM rg.limits
WHERE user_id = $1 AND cancelled_at IS NULL ORDER BY period, effective_from;

-- name: InsertRestriction :exec
INSERT INTO rg.restrictions (user_id, kind, starts_at, ends_at) VALUES ($1, $2, $3, $4);

-- name: ActiveRestrictions :many
SELECT kind, starts_at, ends_at FROM rg.restrictions
WHERE user_id = $1 AND starts_at <= $2 AND (ends_at IS NULL OR ends_at > $2);
