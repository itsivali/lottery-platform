-- name: LockUserKYC :one
SELECT kyc_status, national_id_hash FROM identity.users WHERE id = $1 FOR UPDATE;

-- name: IDHashOwner :one
SELECT id FROM identity.users WHERE national_id_hash = $1 AND id <> $2;

-- name: InsertKYCCheck :one
INSERT INTO kyc.kyc_checks (user_id, supersedes_id, id_number_enc, id_number_hash, full_name_enc, date_of_birth_enc,
                            name_match, dob_match, is_adult, status, provider, provider_ref, reviewer_id, reviewer_reason, created_at)
VALUES (sqlc.arg(user_id), sqlc.narg(supersedes_id), sqlc.arg(id_number_enc), sqlc.arg(id_number_hash), sqlc.arg(full_name_enc),
        sqlc.arg(date_of_birth_enc), sqlc.narg(name_match), sqlc.narg(dob_match), sqlc.narg(is_adult), sqlc.arg(status),
        sqlc.arg(provider), sqlc.narg(provider_ref), sqlc.narg(reviewer_id), sqlc.narg(reviewer_reason), sqlc.arg(created_at))
RETURNING id;

-- name: SetUserKYC :exec
UPDATE identity.users
   SET kyc_status = sqlc.arg(kyc_status), national_id_hash = sqlc.narg(national_id_hash), updated_at = sqlc.arg(at)
 WHERE id = sqlc.arg(id);

-- name: LatestKYCCheck :one
SELECT id, status, id_number_enc, id_number_hash, full_name_enc, date_of_birth_enc, provider, created_at FROM kyc.kyc_checks
WHERE user_id = $1 ORDER BY created_at DESC, id DESC LIMIT 1;

-- name: GetUserKYCStatus :one
SELECT kyc_status FROM identity.users WHERE id = $1;
