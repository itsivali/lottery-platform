-- +goose Up
CREATE TABLE identity.users (
  id                 uuid PRIMARY KEY DEFAULT platform.uuid_v7(),
  msisdn             text NOT NULL CHECK (msisdn ~ '^254[17][0-9]{8}$'),
  national_id_hash   bytea,
  kyc_status         text NOT NULL DEFAULT 'none'
                       CHECK (kyc_status IN ('none','pending','verified','manual_review','rejected')),
  status             text NOT NULL DEFAULT 'active' CHECK (status IN ('active','suspended','closed')),
  language           text NOT NULL DEFAULT 'en' CHECK (language IN ('en','sw')),
  payout_msisdn      text CHECK (payout_msisdn ~ '^254[17][0-9]{8}$'),
  payout_hold_until  timestamptz,
  created_at         timestamptz NOT NULL DEFAULT now(),
  updated_at         timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT users_msisdn_key UNIQUE (msisdn),
  CONSTRAINT users_national_id_hash_key UNIQUE (national_id_hash),
  CONSTRAINT users_verified_has_identity CHECK (kyc_status <> 'verified' OR national_id_hash IS NOT NULL)
);

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

CREATE TABLE identity.auth_identities (
  id          uuid PRIMARY KEY DEFAULT platform.uuid_v7(),
  user_id     uuid NOT NULL REFERENCES identity.users (id),
  provider    text NOT NULL CHECK (provider IN ('google','apple')),
  subject     text NOT NULL CHECK (subject <> ''),
  created_at  timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT auth_identities_provider_subject_key UNIQUE (provider, subject)
);
CREATE INDEX auth_identities_user_idx ON identity.auth_identities (user_id);

CREATE TABLE identity.devices (
  id                   uuid PRIMARY KEY DEFAULT platform.uuid_v7(),
  user_id              uuid NOT NULL REFERENCES identity.users (id),
  fingerprint_hash     bytea NOT NULL,
  platform             text NOT NULL CHECK (platform IN ('android','ios','web')),
  attestation_verdict  text NOT NULL DEFAULT 'none' CHECK (attestation_verdict IN ('none','passed','failed')),
  first_seen_at        timestamptz NOT NULL DEFAULT now(),
  last_seen_at         timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT devices_user_fingerprint_key UNIQUE (user_id, fingerprint_hash)
);
-- device-to-account cap is enforced in the service using this index
CREATE INDEX devices_fingerprint_idx ON identity.devices (fingerprint_hash);

CREATE TABLE identity.sessions (
  id              uuid PRIMARY KEY DEFAULT platform.uuid_v7(),
  user_id         uuid NOT NULL REFERENCES identity.users (id),
  device_id       uuid REFERENCES identity.devices (id),
  created_at      timestamptz NOT NULL DEFAULT now(),
  revoked_at      timestamptz,
  revoked_reason  text CHECK (revoked_reason IN ('logout','reuse_detected','admin','self_exclusion')),
  CONSTRAINT sessions_revoked_has_reason CHECK ((revoked_at IS NULL) = (revoked_reason IS NULL))
);
CREATE INDEX sessions_user_idx ON identity.sessions (user_id);

-- Refresh tokens are opaque; only the hash is stored. Each rotation links to its parent so a reused (already
-- rotated) token can revoke the whole chain.
CREATE TABLE identity.refresh_tokens (
  id          uuid PRIMARY KEY DEFAULT platform.uuid_v7(),
  session_id  uuid NOT NULL REFERENCES identity.sessions (id),
  parent_id   uuid REFERENCES identity.refresh_tokens (id),
  token_hash  bytea NOT NULL,
  issued_at   timestamptz NOT NULL DEFAULT now(),
  expires_at  timestamptz NOT NULL,
  used_at     timestamptz,
  CONSTRAINT refresh_tokens_hash_key UNIQUE (token_hash),
  CONSTRAINT refresh_tokens_expiry_after_issue CHECK (expires_at > issued_at)
);
CREATE INDEX refresh_tokens_session_idx ON identity.refresh_tokens (session_id);

-- KYC. Append-only: a review is a new row that supersedes an earlier one.
CREATE TABLE kyc.kyc_checks (
  id                 uuid PRIMARY KEY DEFAULT platform.uuid_v7(),
  user_id            uuid NOT NULL REFERENCES identity.users (id),
  supersedes_id      uuid REFERENCES kyc.kyc_checks (id),
  id_number_enc      bytea NOT NULL,      -- AES-256-GCM envelope; key-encryption key never enters the database
  id_number_hash     bytea NOT NULL,
  full_name_enc      bytea NOT NULL,
  date_of_birth_enc  bytea NOT NULL,
  name_match         boolean,
  dob_match          boolean,
  is_adult           boolean,
  status             text NOT NULL CHECK (status IN ('verified','mismatch','manual_review','rejected','error')),
  provider           text NOT NULL,
  provider_ref       text,
  reviewer_id        text,
  reviewer_reason    text,
  created_at         timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT kyc_review_has_reviewer CHECK (supersedes_id IS NULL OR (reviewer_id IS NOT NULL AND reviewer_reason IS NOT NULL))
);
CREATE INDEX kyc_checks_user_idx ON kyc.kyc_checks (user_id, created_at DESC);
CREATE TRIGGER kyc_checks_append_only BEFORE UPDATE OR DELETE ON kyc.kyc_checks
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
CREATE TRIGGER kyc_checks_no_truncate BEFORE TRUNCATE ON kyc.kyc_checks
  FOR EACH STATEMENT EXECUTE FUNCTION platform.forbid_mutation();

-- Responsible gaming. Limits are versioned: a decrease is effective immediately, an increase after cooling-off.
CREATE TABLE rg.limits (
  id              uuid PRIMARY KEY DEFAULT platform.uuid_v7(),
  user_id         uuid NOT NULL REFERENCES identity.users (id),
  period          text NOT NULL CHECK (period IN ('daily','weekly','monthly')),
  amount_kes      bigint NOT NULL CHECK (amount_kes > 0),
  effective_from  timestamptz NOT NULL,
  requested_at    timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT limits_user_period_from_key UNIQUE (user_id, period, effective_from)
);

-- Time-outs and self-exclusions. Rows are never shortened or removed; permanent exclusion has no end.
CREATE TABLE rg.restrictions (
  id          uuid PRIMARY KEY DEFAULT platform.uuid_v7(),
  user_id     uuid NOT NULL REFERENCES identity.users (id),
  kind        text NOT NULL CHECK (kind IN ('timeout','self_exclusion')),
  starts_at   timestamptz NOT NULL,
  ends_at     timestamptz,
  created_at  timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT restrictions_timeout_has_end CHECK (kind = 'self_exclusion' OR ends_at IS NOT NULL),
  CONSTRAINT restrictions_end_after_start CHECK (ends_at IS NULL OR ends_at > starts_at)
);
CREATE INDEX restrictions_user_idx ON rg.restrictions (user_id, starts_at);
CREATE TRIGGER restrictions_append_only BEFORE UPDATE OR DELETE ON rg.restrictions
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
CREATE TRIGGER restrictions_no_truncate BEFORE TRUNCATE ON rg.restrictions
  FOR EACH STATEMENT EXECUTE FUNCTION platform.forbid_mutation();

-- +goose Down
DROP TABLE IF EXISTS rg.restrictions, rg.limits, kyc.kyc_checks, identity.refresh_tokens, identity.sessions,
  identity.devices, identity.auth_identities, identity.phone_otps, identity.users;
