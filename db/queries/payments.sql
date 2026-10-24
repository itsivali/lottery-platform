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

-- C2B (paybill). A confirmation is stored once per receipt; a repeat returns no row.
-- name: InsertC2BConfirmation :one
INSERT INTO payments.c2b_confirmations (trans_id, shortcode, bill_ref, payer_msisdn, amount_kes, raw_confirmation, created_at, updated_at)
VALUES (sqlc.arg(trans_id), sqlc.arg(shortcode), sqlc.arg(bill_ref), sqlc.narg(payer_msisdn), sqlc.arg(amount_kes), sqlc.arg(raw), sqlc.arg(at), sqlc.arg(at))
ON CONFLICT (trans_id) DO NOTHING
RETURNING id;

-- name: LockC2BByTransID :one
SELECT id, shortcode, bill_ref, payer_msisdn, amount_kes, status FROM payments.c2b_confirmations WHERE trans_id = $1 FOR UPDATE;

-- name: MarkC2BConflicting :exec
UPDATE payments.c2b_confirmations SET conflicting = true, updated_at = sqlc.arg(at) WHERE id = sqlc.arg(id);

-- name: GetC2B :one
SELECT id, trans_id, shortcode, bill_ref, payer_msisdn, amount_kes, raw_confirmation, conflicting, status, status_requests,
       status_requested_at, originator_conversation_id, status_result, outcome_reason, payment_id, unmatched_id, created_at, updated_at
FROM payments.c2b_confirmations WHERE id = $1;

-- name: LockC2B :one
SELECT id, trans_id, shortcode, bill_ref, payer_msisdn, amount_kes, raw_confirmation, conflicting, status, status_requests,
       status_requested_at, originator_conversation_id, status_result, outcome_reason, payment_id, unmatched_id, created_at, updated_at
FROM payments.c2b_confirmations WHERE id = $1 FOR UPDATE;

-- name: LockC2BForResult :one
-- a status result names the request it answers; an answer to an earlier request is matched by its receipt instead
SELECT id, status FROM payments.c2b_confirmations
WHERE originator_conversation_id = sqlc.arg(originator) OR (sqlc.arg(receipt)::text <> '' AND trans_id = sqlc.arg(receipt)::text)
ORDER BY (originator_conversation_id = sqlc.arg(originator)) DESC LIMIT 1 FOR UPDATE;

-- name: MarkC2BStatusRequested :execrows
UPDATE payments.c2b_confirmations
   SET status = 'status_requested', status_requests = status_requests + 1, status_requested_at = sqlc.arg(at),
       originator_conversation_id = sqlc.arg(originator), updated_at = sqlc.arg(at)
 WHERE id = sqlc.arg(id) AND status IN ('received','status_requested');

-- name: RecordC2BStatusResult :exec
UPDATE payments.c2b_confirmations
   SET status = 'status_received', status_result = sqlc.arg(result), updated_at = sqlc.arg(at)
 WHERE id = sqlc.arg(id);

-- name: SetC2BOutcome :exec
UPDATE payments.c2b_confirmations
   SET status = sqlc.arg(status), outcome_reason = sqlc.narg(reason), payment_id = sqlc.narg(payment_id),
       unmatched_id = sqlc.narg(unmatched_id), updated_at = sqlc.arg(at)
 WHERE id = sqlc.arg(id);

-- name: RecheckC2B :execrows
UPDATE payments.c2b_confirmations
   SET status = 'received', status_result = NULL, originator_conversation_id = NULL, outcome_reason = NULL,
       status_requests = 0, status_requested_at = NULL, updated_at = sqlc.arg(at)
 WHERE trans_id = sqlc.arg(trans_id) AND status = 'unverified';

-- Confirmations whose verification may have stalled: never asked about, asked without an answer for a while, or
-- answered without being settled.
-- name: StaleC2B :many
SELECT id FROM payments.c2b_confirmations
WHERE status IN ('received','status_requested','status_received') AND coalesce(status_requested_at, updated_at) < $1
ORDER BY created_at LIMIT 100;

-- name: InsertC2BPayment :one
INSERT INTO payments.payments (user_id, channel, amount_requested_kes, msisdn, idempotency_key, created_at, updated_at)
VALUES (sqlc.arg(user_id), 'c2b', sqlc.arg(amount_kes), sqlc.arg(msisdn), sqlc.arg(idempotency_key), sqlc.arg(at), sqlc.arg(at))
RETURNING id;

-- name: InsertUnmatched :one
INSERT INTO payments.c2b_unmatched (mpesa_receipt, msisdn, account_ref, amount_kes, raw_payload, reason, created_at)
VALUES (sqlc.arg(receipt), sqlc.narg(msisdn), sqlc.arg(account_ref), sqlc.arg(amount_kes), sqlc.arg(raw), sqlc.arg(reason), sqlc.arg(at))
RETURNING id;

-- name: LockUnmatchedByReceipt :one
SELECT id, mpesa_receipt, account_ref, amount_kes, status, reason FROM payments.c2b_unmatched WHERE mpesa_receipt = $1 FOR UPDATE;

-- name: ResolveUnmatched :exec
UPDATE payments.c2b_unmatched
   SET status = 'resolved', payment_id = sqlc.arg(payment_id), resolved_at = sqlc.arg(at), resolved_by = sqlc.arg(operator)
 WHERE id = sqlc.arg(id) AND status = 'open';

-- name: RefundUnmatched :exec
UPDATE payments.c2b_unmatched
   SET status = 'refunded', refund_ref = sqlc.arg(refund_ref), resolved_at = sqlc.arg(at), resolved_by = sqlc.arg(operator)
 WHERE id = sqlc.arg(id) AND status = 'open';

-- name: SuspenseOpenTotal :one
SELECT coalesce(sum(amount_kes), 0)::bigint FROM payments.c2b_unmatched WHERE status = 'open';

-- name: OpenSuspense :many
SELECT mpesa_receipt FROM payments.c2b_unmatched WHERE status = 'open' ORDER BY created_at;

-- name: OverdueC2B :many
SELECT trans_id FROM payments.c2b_confirmations
WHERE status IN ('received','status_requested','status_received') AND created_at < $1 ORDER BY created_at;

-- name: UnverifiedC2B :many
SELECT trans_id FROM payments.c2b_confirmations WHERE status = 'unverified' ORDER BY created_at;

-- name: SuspenseWithoutJournal :many
-- every suspense item has its parking journal, and a settled one its release or refund journal
SELECT u.id::text FROM payments.c2b_unmatched u
WHERE NOT EXISTS (SELECT 1 FROM ledger.ledger_journals j WHERE j.kind = 'c2b_suspense' AND j.reference_type = 'c2b_unmatched' AND j.reference_id = u.id)
   OR (u.status <> 'open') <> EXISTS (SELECT 1 FROM ledger.ledger_journals j
        WHERE j.kind IN ('c2b_suspense_release','c2b_suspense_refund') AND j.reference_type = 'c2b_unmatched' AND j.reference_id = u.id)
ORDER BY 1;

-- name: ReceiptCredited :one
SELECT EXISTS (SELECT 1 FROM payments.payments WHERE mpesa_receipt = $1);

-- name: ConflictsAfterCredit :many
SELECT trans_id FROM payments.c2b_confirmations WHERE conflicting AND status = 'credited' ORDER BY created_at;

-- What a player's live stream starts from: payments still being settled, and those that changed recently.
-- name: RecentPayments :many
SELECT id, status, amount_requested_kes, amount_confirmed_kes, created_at
FROM payments.payments
WHERE user_id = sqlc.arg(user_id)
  AND (updated_at >= sqlc.arg(since) OR status IN ('initiated','stk_sent','callback_received','verified'))
ORDER BY created_at DESC LIMIT 20;
