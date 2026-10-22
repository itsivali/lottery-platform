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

-- name: LockPaymentByReceipt :one
SELECT id, user_id, status, amount_confirmed_kes FROM payments.payments WHERE mpesa_receipt = $1 FOR UPDATE;

-- What the deposit of a payment deducted from the player's credit (the deposit_excise line; 0 when absorbed).
-- name: DepositExciseDeducted :one
SELECT coalesce(sum(e.amount_kes), 0)::bigint
FROM ledger.ledger_entries e
JOIN ledger.ledger_journals j ON j.id = e.journal_id
JOIN ledger.ledger_accounts a ON a.id = e.account_id
WHERE j.kind = 'deposit' AND j.reference_type = 'payment' AND j.reference_id = sqlc.arg(payment_id)::uuid
  AND a.code = 'WALLET_LIABILITY' AND e.direction = 'D';

-- name: MarkReversed :exec
UPDATE payments.payments
   SET status = 'reversed', reversed_at = sqlc.arg(at), reversal_reason = sqlc.arg(reason), updated_at = sqlc.arg(at)
 WHERE id = sqlc.arg(id) AND status = 'credited';

-- Reconciliation. Each "mismatch" query returns the ids that break an invariant; each must return nothing.

-- name: CreditedWithoutDeposit :many
SELECT p.id FROM payments.payments p
WHERE p.status IN ('credited','reversed')
  AND NOT EXISTS (SELECT 1 FROM ledger.ledger_journals j WHERE j.kind = 'deposit' AND j.reference_type = 'payment' AND j.reference_id = p.id)
ORDER BY p.id;

-- name: DepositWithoutCredit :many
SELECT j.reference_id::text FROM ledger.ledger_journals j
WHERE j.kind = 'deposit' AND j.reference_type = 'payment'
  AND NOT EXISTS (SELECT 1 FROM payments.payments p WHERE p.id = j.reference_id AND p.status IN ('credited','reversed'))
ORDER BY 1;

-- name: DepositAmountMismatch :many
SELECT p.id FROM payments.payments p
JOIN ledger.ledger_journals j ON j.kind = 'deposit' AND j.reference_type = 'payment' AND j.reference_id = p.id
JOIN ledger.ledger_entries e ON e.journal_id = j.id AND e.direction = 'D'
JOIN ledger.ledger_accounts a ON a.id = e.account_id AND a.code = 'MPESA_CLEARING'
GROUP BY p.id, p.amount_confirmed_kes
HAVING sum(e.amount_kes) IS DISTINCT FROM p.amount_confirmed_kes
ORDER BY p.id;

-- name: ReversalMismatch :many
SELECT p.id::text FROM payments.payments p
WHERE (p.status = 'reversed') <> EXISTS (SELECT 1 FROM ledger.ledger_journals j
  WHERE j.kind = 'deposit_reversal' AND j.reference_type = 'payment' AND j.reference_id = p.id)
ORDER BY 1;

-- name: AccountBalance :one
-- debits minus credits on an account
SELECT coalesce(sum(CASE e.direction WHEN 'D' THEN e.amount_kes ELSE -e.amount_kes END), 0)::bigint
FROM ledger.ledger_entries e JOIN ledger.ledger_accounts a ON a.id = e.account_id WHERE a.code = $1;

-- name: CreditedTotal :one
SELECT coalesce(sum(amount_confirmed_kes), 0)::bigint FROM payments.payments WHERE status = 'credited';

-- name: WalletsTotal :one
SELECT coalesce(sum(balance_kes), 0)::bigint FROM wallet.wallets;

-- name: UnconfirmedPayments :many
SELECT id FROM payments.payments WHERE status = 'unconfirmed' ORDER BY created_at;

-- name: OverduePending :many
SELECT id FROM payments.payments
WHERE status IN ('initiated','stk_sent','callback_received') AND created_at < $1 ORDER BY created_at;

-- name: RecentOrphanCallbacks :many
SELECT DISTINCT checkout_request_id FROM payments.stk_orphan_callbacks WHERE received_at >= $1 ORDER BY 1;

-- name: PlayersInDebt :many
SELECT e.user_id::text AS user_id,
       sum(CASE e.direction WHEN 'D' THEN e.amount_kes ELSE -e.amount_kes END)::bigint AS debt
FROM ledger.ledger_entries e JOIN ledger.ledger_accounts a ON a.id = e.account_id
WHERE a.code = 'PLAYER_DEBT' AND e.user_id IS NOT NULL
GROUP BY e.user_id
HAVING sum(CASE e.direction WHEN 'D' THEN e.amount_kes ELSE -e.amount_kes END) > 0
ORDER BY 1;

-- name: InsertReconciliationRun :exec
INSERT INTO payments.reconciliation_runs (ran_at, ok, report) VALUES ($1, $2, $3);
