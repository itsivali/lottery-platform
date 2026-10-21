-- name: InsertOTP :exec
INSERT INTO identity.phone_otps (msisdn, purpose, code_hmac, expires_at, ip, created_at)
VALUES (sqlc.arg(msisdn), sqlc.arg(purpose), sqlc.arg(code_hmac), sqlc.arg(expires_at), sqlc.narg(ip)::inet, sqlc.arg(created_at));

-- name: LatestOTPCreatedAt :one
SELECT created_at FROM identity.phone_otps
WHERE msisdn = $1 AND purpose = $2
ORDER BY created_at DESC LIMIT 1;

-- name: LockActiveOTP :one
SELECT id, code_hmac, attempts FROM identity.phone_otps
WHERE msisdn = $1 AND purpose = $2 AND consumed_at IS NULL AND expires_at > $3 AND attempts < 5
ORDER BY created_at DESC LIMIT 1
FOR UPDATE;

-- name: BumpOTPAttempts :exec
UPDATE identity.phone_otps SET attempts = attempts + 1 WHERE id = $1 AND attempts < 5;

-- name: ConsumeOTP :exec
UPDATE identity.phone_otps SET consumed_at = $2 WHERE id = $1 AND consumed_at IS NULL;

-- name: GetUserByMSISDN :one
SELECT id, kyc_status, status FROM identity.users WHERE msisdn = $1;

-- name: InsertUser :one
INSERT INTO identity.users (msisdn) VALUES ($1)
ON CONFLICT (msisdn) DO UPDATE SET msisdn = EXCLUDED.msisdn
RETURNING id, (xmax = 0) AS created;

-- name: GetUser :one
SELECT id, msisdn, kyc_status, status, language FROM identity.users WHERE id = $1;

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
