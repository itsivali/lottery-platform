-- +goose Up
-- Phone ownership is proved by an M-Pesa STK Push confirmation (the first top-up), not by an SMS code, so there is no
-- OTP table any more. Accounts are created with a phone number and a password. A number is unique only once verified:
-- until then it is just a claim, so nobody can lock the real owner out by registering their number first.
ALTER TABLE identity.users
  ADD COLUMN password_hash       text,
  ADD COLUMN password_changed_at timestamptz,
  ADD COLUMN msisdn_verified_at  timestamptz;

ALTER TABLE identity.users DROP CONSTRAINT users_msisdn_key;
CREATE INDEX users_msisdn_idx ON identity.users (msisdn, created_at DESC);
CREATE UNIQUE INDEX users_msisdn_verified_key ON identity.users (msisdn) WHERE msisdn_verified_at IS NOT NULL;
ALTER TABLE identity.users ADD CONSTRAINT users_verified_phone_has_password
  CHECK (msisdn_verified_at IS NULL OR password_hash IS NOT NULL);

DROP TABLE identity.phone_otps;

ALTER TABLE identity.sessions DROP CONSTRAINT sessions_revoked_reason_check;
ALTER TABLE identity.sessions ADD CONSTRAINT sessions_revoked_reason_check
  CHECK (revoked_reason IN ('logout','reuse_detected','admin','self_exclusion','password_change'));

-- +goose Down
CREATE TABLE identity.phone_otps (
  id           uuid PRIMARY KEY DEFAULT platform.uuid_v7(),
  msisdn       text NOT NULL CHECK (msisdn ~ '^254[17][0-9]{8}$'),
  purpose      text NOT NULL CHECK (purpose IN ('signup','login','payout_change','sso_link')),
  code_hmac    bytea NOT NULL,
  attempts     smallint NOT NULL DEFAULT 0,
  expires_at   timestamptz NOT NULL,
  consumed_at  timestamptz,
  ip           inet,
  created_at   timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT phone_otps_attempts_max CHECK (attempts BETWEEN 0 AND 5),
  CONSTRAINT phone_otps_expiry_after_creation CHECK (expires_at > created_at)
);
CREATE INDEX phone_otps_msisdn_created_idx ON identity.phone_otps (msisdn, created_at DESC);

UPDATE identity.sessions SET revoked_reason = 'admin' WHERE revoked_reason = 'password_change';
ALTER TABLE identity.sessions DROP CONSTRAINT sessions_revoked_reason_check;
ALTER TABLE identity.sessions ADD CONSTRAINT sessions_revoked_reason_check
  CHECK (revoked_reason IN ('logout','reuse_detected','admin','self_exclusion'));

ALTER TABLE identity.users DROP CONSTRAINT users_verified_phone_has_password;
DROP INDEX identity.users_msisdn_verified_key;
DROP INDEX identity.users_msisdn_idx;
ALTER TABLE identity.users ADD CONSTRAINT users_msisdn_key UNIQUE (msisdn);
ALTER TABLE identity.users DROP COLUMN msisdn_verified_at, DROP COLUMN password_changed_at, DROP COLUMN password_hash;
