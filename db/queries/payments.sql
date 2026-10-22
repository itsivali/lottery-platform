-- name: InsertTopUp :one
INSERT INTO payments.payments (user_id, channel, amount_requested_kes, msisdn, idempotency_key, created_at, updated_at)
VALUES (sqlc.arg(user_id), 'stk', sqlc.arg(amount_kes), sqlc.arg(msisdn), sqlc.arg(idempotency_key), sqlc.arg(at), sqlc.arg(at))
RETURNING id;

-- name: GetPaymentByKey :one
SELECT id FROM payments.payments WHERE user_id = $1 AND idempotency_key = $2;

-- name: GetPayment :one
SELECT id, user_id, status, amount_requested_kes, amount_confirmed_kes, msisdn, checkout_request_id, mpesa_receipt,
       raw_callback, failure_reason, result_code, created_at, updated_at
FROM payments.payments WHERE id = $1;

-- name: LockPayment :one
SELECT id, user_id, status, amount_requested_kes, amount_confirmed_kes, msisdn, checkout_request_id, mpesa_receipt,
       raw_callback, failure_reason, result_code, created_at, updated_at
FROM payments.payments WHERE id = $1 FOR UPDATE;

-- name: LockPaymentByCheckout :one
SELECT id, status FROM payments.payments WHERE checkout_request_id = $1 FOR UPDATE;

-- name: MarkSTKSent :execrows
UPDATE payments.payments
   SET status = 'stk_sent', merchant_request_id = sqlc.arg(merchant_request_id),
       checkout_request_id = sqlc.arg(checkout_request_id), updated_at = sqlc.arg(at)
 WHERE id = sqlc.arg(id) AND status = 'initiated';

-- name: RecordCallback :exec
UPDATE payments.payments
   SET status = CASE WHEN status IN ('initiated','stk_sent') THEN 'callback_received' ELSE status END,
       raw_callback = sqlc.arg(raw_callback), updated_at = sqlc.arg(at)
 WHERE id = sqlc.arg(id);

-- name: SetPaymentOutcome :exec
UPDATE payments.payments
   SET status = sqlc.arg(status), failure_reason = sqlc.narg(failure_reason), result_code = sqlc.narg(result_code),
       updated_at = sqlc.arg(at)
 WHERE id = sqlc.arg(id);

-- name: MarkCredited :exec
UPDATE payments.payments
   SET status = 'credited', amount_confirmed_kes = sqlc.arg(amount_kes), mpesa_receipt = sqlc.arg(receipt),
       result_code = 0, updated_at = sqlc.arg(at)
 WHERE id = sqlc.arg(id);

-- name: ReceiptUsedElsewhere :one
SELECT EXISTS (SELECT 1 FROM payments.payments WHERE mpesa_receipt = $1 AND id <> $2);

-- name: InsertOrphanCallback :exec
INSERT INTO payments.stk_orphan_callbacks (checkout_request_id, raw_payload) VALUES ($1, $2);

-- Deposits that count against responsible-gaming limits: credited ones, plus top-ups still in flight so that
-- parallel requests cannot slip past a limit.
-- name: DepositedSince :one
SELECT coalesce(sum(coalesce(amount_confirmed_kes, amount_requested_kes)), 0)::bigint
FROM payments.payments
WHERE user_id = $1 AND created_at >= $2
  AND status IN ('initiated','stk_sent','callback_received','verified','credited','unconfirmed');

-- name: StalePending :many
SELECT id FROM payments.payments
WHERE status IN ('initiated','stk_sent','callback_received') AND created_at < $1
ORDER BY created_at LIMIT 100;

-- A callback that arrived before the push job recorded its checkout id was kept as an orphan.
-- name: OrphanCallback :one
SELECT raw_payload FROM payments.stk_orphan_callbacks WHERE checkout_request_id = $1 ORDER BY received_at DESC LIMIT 1;
