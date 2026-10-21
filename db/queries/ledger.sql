-- name: ListAccounts :many
SELECT code, id FROM ledger.ledger_accounts;

-- name: InsertJournal :one
INSERT INTO ledger.ledger_journals (kind, reference_type, reference_id)
VALUES ($1, $2, $3)
RETURNING id;

-- name: InsertEntry :exec
INSERT INTO ledger.ledger_entries (journal_id, account_id, user_id, direction, amount_kes)
VALUES ($1, $2, $3, $4, $5);

-- name: ActiveTaxRule :one
SELECT code, rate::text AS rate, rounding
FROM ledger.tax_rules
WHERE code = $1 AND effective_from <= $2
ORDER BY effective_from DESC
LIMIT 1;
