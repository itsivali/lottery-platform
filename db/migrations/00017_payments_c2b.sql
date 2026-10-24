-- +goose Up
-- Phase 2 slice 3: paybill (C2B) payments. A confirmation from Daraja is a claim: it is stored here, verified with the
-- Transaction Status API, and only then credited to the player whose proved phone is the account number, or parked in
-- suspense (payments.c2b_unmatched + the SUSPENSE account) for an operator.
CREATE TABLE payments.c2b_confirmations (
  id                          uuid PRIMARY KEY DEFAULT platform.uuid_v7(),
  trans_id                    text NOT NULL CHECK (trans_id ~ '^[A-Z0-9]{10}$'),
  shortcode                   text NOT NULL,
  bill_ref                    text NOT NULL,
  payer_msisdn                text CHECK (payer_msisdn ~ '^254[17][0-9]{8}$'),  -- NULL when Daraja masks it
  amount_kes                  bigint NOT NULL CHECK (amount_kes > 0),
  raw_confirmation            jsonb NOT NULL,
  -- a later confirmation for the same receipt that disagrees with the first (a forgery or a replay with edits)
  conflicting                 boolean NOT NULL DEFAULT false,
  status                      text NOT NULL DEFAULT 'received'
                                CHECK (status IN ('received','status_requested','status_received','credited','suspense','unverified')),
  status_requests             integer NOT NULL DEFAULT 0 CHECK (status_requests >= 0),
  status_requested_at         timestamptz,
  originator_conversation_id  text,
  status_result               jsonb,
  outcome_reason              text,
  payment_id                  uuid REFERENCES payments.payments (id),
  unmatched_id                uuid REFERENCES payments.c2b_unmatched (id),
  created_at                  timestamptz NOT NULL DEFAULT now(),
  updated_at                  timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT c2b_confirmations_trans_id_key UNIQUE (trans_id),
  CONSTRAINT c2b_confirmations_originator_key UNIQUE (originator_conversation_id),
  CONSTRAINT c2b_confirmations_credited_has_payment CHECK (status <> 'credited' OR payment_id IS NOT NULL),
  CONSTRAINT c2b_confirmations_suspense_has_item CHECK (status <> 'suspense' OR unmatched_id IS NOT NULL)
);
CREATE INDEX c2b_confirmations_open_idx ON payments.c2b_confirmations (created_at)
  WHERE status IN ('received','status_requested','status_received');

-- Suspense items: why they are there and how an operator settled them.
ALTER TABLE payments.c2b_unmatched
  ADD COLUMN reason       text NOT NULL DEFAULT 'unmatched',
  ADD COLUMN payment_id   uuid REFERENCES payments.payments (id),
  ADD COLUMN refund_ref   text,
  ADD COLUMN resolved_by  text,
  ADD CONSTRAINT c2b_unmatched_resolved_has_payment CHECK (status <> 'resolved' OR (payment_id IS NOT NULL AND resolved_at IS NOT NULL AND resolved_by IS NOT NULL)),
  ADD CONSTRAINT c2b_unmatched_refunded_has_ref CHECK (status <> 'refunded' OR (refund_ref IS NOT NULL AND resolved_at IS NOT NULL AND resolved_by IS NOT NULL));

-- +goose StatementBegin
DO $$
BEGIN
  IF (SELECT count(*) FROM pg_roles WHERE rolname IN ('app_migrator','app_api','app_worker','app_readonly')) < 4
     OR NOT pg_has_role(current_user, 'app_migrator', 'SET') THEN
    RAISE WARNING 'application roles unavailable: skipping grants (SINGLE-ROLE MODE)';
    RETURN;
  END IF;
  GRANT SELECT, INSERT, UPDATE ON payments.c2b_confirmations TO app_api, app_worker;
  GRANT SELECT ON payments.c2b_confirmations TO app_readonly;
  ALTER TABLE payments.c2b_confirmations OWNER TO app_migrator;
END $$;
-- +goose StatementEnd

-- +goose Down
DROP TABLE payments.c2b_confirmations;
ALTER TABLE payments.c2b_unmatched
  DROP CONSTRAINT c2b_unmatched_refunded_has_ref, DROP CONSTRAINT c2b_unmatched_resolved_has_payment,
  DROP COLUMN resolved_by, DROP COLUMN refund_ref, DROP COLUMN payment_id, DROP COLUMN reason;
