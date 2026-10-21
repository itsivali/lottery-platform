-- name: UpsertDevice :one
INSERT INTO identity.devices (user_id, fingerprint_hash, platform, attestation_verdict, first_seen_at, last_seen_at)
VALUES (sqlc.arg(user_id), sqlc.arg(fingerprint_hash), sqlc.arg(platform), sqlc.arg(attestation_verdict), sqlc.arg(at), sqlc.arg(at))
ON CONFLICT (user_id, fingerprint_hash) DO UPDATE
   SET last_seen_at = EXCLUDED.last_seen_at, attestation_verdict = EXCLUDED.attestation_verdict
RETURNING id;

-- name: CountOtherAccountsOnDevice :one
SELECT count(DISTINCT user_id) FROM identity.devices WHERE fingerprint_hash = $1 AND user_id <> $2;

-- name: InsertSession :one
INSERT INTO identity.sessions (user_id, device_id, created_at) VALUES ($1, $2, $3) RETURNING id;

-- name: GetSession :one
SELECT s.id, s.user_id, s.device_id, s.revoked_at, d.fingerprint_hash
FROM identity.sessions s LEFT JOIN identity.devices d ON d.id = s.device_id
WHERE s.id = $1;

-- name: RevokeSession :exec
UPDATE identity.sessions SET revoked_at = sqlc.arg(at), revoked_reason = sqlc.arg(reason)
WHERE id = sqlc.arg(id) AND revoked_at IS NULL;

-- name: InsertRefreshToken :exec
INSERT INTO identity.refresh_tokens (session_id, parent_id, token_hash, issued_at, expires_at)
VALUES (sqlc.arg(session_id), sqlc.narg(parent_id), sqlc.arg(token_hash), sqlc.arg(issued_at), sqlc.arg(expires_at));

-- name: LockRefreshToken :one
SELECT id, session_id, expires_at, used_at FROM identity.refresh_tokens WHERE token_hash = $1 FOR UPDATE;

-- name: MarkRefreshUsed :exec
UPDATE identity.refresh_tokens SET used_at = $2 WHERE id = $1;

-- name: InsertAuthIdentity :exec
INSERT INTO identity.auth_identities (user_id, provider, subject) VALUES ($1, $2, $3);

-- name: FindAuthIdentity :one
SELECT user_id FROM identity.auth_identities WHERE provider = $1 AND subject = $2;

-- name: GetSessionDevice :one
SELECT d.platform, d.attestation_verdict
FROM identity.sessions s JOIN identity.devices d ON d.id = s.device_id
WHERE s.id = $1;

-- name: InsertAccount :one
INSERT INTO identity.users (msisdn, password_hash, password_changed_at, created_at, updated_at)
VALUES (sqlc.arg(msisdn), sqlc.arg(password_hash), sqlc.arg(at), sqlc.arg(at), sqlc.arg(at))
RETURNING id;

-- name: VerifiedAccountExists :one
SELECT EXISTS (SELECT 1 FROM identity.users WHERE msisdn = $1 AND msisdn_verified_at IS NOT NULL) ::boolean AS exists;

-- A number with a proved owner admits only that owner; otherwise the newest few unproved claims are tried.
-- name: LoginCandidates :many
SELECT id, password_hash, status, (msisdn_verified_at IS NOT NULL)::boolean AS phone_verified
FROM identity.users
WHERE msisdn = $1 AND password_hash IS NOT NULL
ORDER BY (msisdn_verified_at IS NULL), created_at DESC
LIMIT 5;

-- name: GetUser :one
SELECT id, msisdn, kyc_status, status, language, password_hash, (msisdn_verified_at IS NOT NULL)::boolean AS phone_verified
FROM identity.users WHERE id = $1;

-- name: SetPassword :exec
UPDATE identity.users SET password_hash = $2, password_changed_at = $3, updated_at = $3 WHERE id = $1;

-- name: RevokeOtherSessions :exec
UPDATE identity.sessions SET revoked_at = sqlc.arg(at), revoked_reason = 'password_change'
WHERE user_id = sqlc.arg(user_id) AND id <> sqlc.arg(keep_session_id) AND revoked_at IS NULL;

-- name: LockUserPhone :one
SELECT msisdn, (msisdn_verified_at IS NOT NULL)::boolean AS verified FROM identity.users WHERE id = $1 FOR UPDATE;

-- name: MarkPhoneVerified :exec
UPDATE identity.users SET msisdn_verified_at = $2, updated_at = $2 WHERE id = $1 AND msisdn_verified_at IS NULL;

