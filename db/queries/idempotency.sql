-- ClaimKey takes the key if it is new, expired, or an in-flight claim abandoned longer than stale_seconds ago.
-- Zero rows returned means someone else owns it (see GetKey).
-- name: ClaimKey :one
INSERT INTO platform.idempotency_keys AS k (principal, key, request_hash, expires_at)
VALUES (sqlc.arg(principal), sqlc.arg(key), sqlc.arg(request_hash), now() + make_interval(secs => sqlc.arg(ttl_seconds)::float8))
ON CONFLICT (principal, key) DO UPDATE
   SET request_hash = EXCLUDED.request_hash,
       response_status = NULL, response_body = NULL, response_content_type = NULL, response_location = NULL,
       created_at = now(), completed_at = NULL, expires_at = EXCLUDED.expires_at
 WHERE k.expires_at <= now()
    OR (k.completed_at IS NULL AND k.created_at <= now() - make_interval(secs => sqlc.arg(stale_seconds)::float8))
RETURNING principal;

-- name: GetKey :one
SELECT request_hash, response_status, response_body, response_content_type, response_location, completed_at
FROM platform.idempotency_keys
WHERE principal = $1 AND key = $2;

-- name: CompleteKey :exec
UPDATE platform.idempotency_keys
   SET response_status = sqlc.arg(status), response_body = sqlc.arg(body),
       response_content_type = sqlc.arg(content_type), response_location = sqlc.arg(location), completed_at = now()
 WHERE principal = sqlc.arg(principal) AND key = sqlc.arg(key) AND request_hash = sqlc.arg(request_hash) AND completed_at IS NULL;

-- ReleaseKey makes an unfinished claim immediately reusable (used after a 5xx or a panic).
-- name: ReleaseKey :exec
UPDATE platform.idempotency_keys SET expires_at = now()
 WHERE principal = $1 AND key = $2 AND request_hash = $3 AND completed_at IS NULL;
