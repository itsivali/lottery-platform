-- name: EnsureWallet :exec
INSERT INTO wallet.wallets (user_id) VALUES ($1) ON CONFLICT (user_id) DO NOTHING;

-- name: CreditWallet :one
UPDATE wallet.wallets SET balance_kes = balance_kes + sqlc.arg(amount_kes), updated_at = now()
WHERE user_id = sqlc.arg(user_id)
RETURNING balance_kes;

-- DebitWallet is the only way money leaves a wallet (invariant 3): one atomic conditional UPDATE.
-- name: DebitWallet :one
UPDATE wallet.wallets SET balance_kes = balance_kes - sqlc.arg(amount_kes), updated_at = now()
WHERE user_id = sqlc.arg(user_id) AND balance_kes >= sqlc.arg(amount_kes)
RETURNING balance_kes;

-- name: LockWalletBalance :one
SELECT balance_kes FROM wallet.wallets WHERE user_id = $1 FOR UPDATE;

-- name: GetBalance :one
SELECT balance_kes FROM wallet.wallets WHERE user_id = $1;

-- name: InsertWalletTx :exec
INSERT INTO wallet.wallet_transactions (user_id, journal_id, kind, amount_kes, reference_type, reference_id)
VALUES ($1, $2, $3, $4, $5, $6);
