-- +goose Up
CREATE TABLE payments.payments (
  id                    uuid PRIMARY KEY DEFAULT platform.uuid_v7(),
  user_id               uuid NOT NULL REFERENCES identity.users (id),
  channel               text NOT NULL CHECK (channel IN ('stk','c2b')),
  status                text NOT NULL DEFAULT 'initiated'
                          CHECK (status IN ('initiated','stk_sent','callback_received','verified','credited','failed','cancelled','expired')),
  amount_requested_kes  bigint NOT NULL CHECK (amount_requested_kes > 0),
  amount_confirmed_kes  bigint CHECK (amount_confirmed_kes > 0),
  msisdn                text CHECK (msisdn ~ '^254[17][0-9]{8}$'),
  merchant_request_id   text,
  checkout_request_id   text,
  mpesa_receipt         text,
  raw_callback          jsonb,
  failure_reason        text,
  idempotency_key       text NOT NULL,
  created_at            timestamptz NOT NULL DEFAULT now(),
  updated_at            timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT payments_mpesa_receipt_key UNIQUE (mpesa_receipt),
  CONSTRAINT payments_checkout_request_id_key UNIQUE (checkout_request_id),
  CONSTRAINT payments_user_idempotency_key UNIQUE (user_id, idempotency_key),
  -- the credited amount is the confirmed amount, never the requested one (invariant 8)
  CONSTRAINT payments_credited_has_confirmation CHECK (
    status NOT IN ('verified','credited') OR (amount_confirmed_kes IS NOT NULL AND mpesa_receipt IS NOT NULL))
);
CREATE INDEX payments_user_idx ON payments.payments (user_id, created_at DESC);
CREATE INDEX payments_pending_idx ON payments.payments (updated_at) WHERE status IN ('initiated','stk_sent','callback_received');

-- C2B confirmations that match no verified account wait here for the back-office.
CREATE TABLE payments.c2b_unmatched (
  id             uuid PRIMARY KEY DEFAULT platform.uuid_v7(),
  mpesa_receipt  text NOT NULL,
  msisdn         text,
  account_ref    text,
  amount_kes     bigint NOT NULL CHECK (amount_kes > 0),
  raw_payload    jsonb NOT NULL,
  status         text NOT NULL DEFAULT 'open' CHECK (status IN ('open','resolved','refunded')),
  created_at     timestamptz NOT NULL DEFAULT now(),
  resolved_at    timestamptz,
  CONSTRAINT c2b_unmatched_receipt_key UNIQUE (mpesa_receipt)
);

CREATE TABLE payouts.payouts (
  id                          uuid PRIMARY KEY DEFAULT platform.uuid_v7(),
  user_id                     uuid NOT NULL REFERENCES identity.users (id),
  kind                        text NOT NULL CHECK (kind IN ('prize','withdrawal')),
  route                       text NOT NULL DEFAULT 'b2c' CHECK (route IN ('b2c','bank')),
  status                      text NOT NULL DEFAULT 'pending'
                                CHECK (status IN ('pending','sent','timed_out','succeeded','failed','manual')),
  gross_kes                   bigint NOT NULL CHECK (gross_kes > 0),
  tax_kes                     bigint NOT NULL DEFAULT 0 CHECK (tax_kes >= 0),
  net_kes                     bigint NOT NULL CHECK (net_kes > 0),
  msisdn                      text CHECK (msisdn ~ '^254[17][0-9]{8}$'),
  draw_result_id              uuid,
  originator_conversation_id  text NOT NULL,
  conversation_id             text,
  mpesa_receipt               text,
  idempotency_key             text NOT NULL,
  created_at                  timestamptz NOT NULL DEFAULT now(),
  updated_at                  timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT payouts_originator_key UNIQUE (originator_conversation_id),
  CONSTRAINT payouts_receipt_key UNIQUE (mpesa_receipt),
  CONSTRAINT payouts_net_is_gross_minus_tax CHECK (net_kes = gross_kes - tax_kes),
  CONSTRAINT payouts_user_idempotency_key UNIQUE (user_id, idempotency_key)
);
-- a prize result can be paid out at most once
CREATE UNIQUE INDEX payouts_draw_result_once ON payouts.payouts (draw_result_id) WHERE draw_result_id IS NOT NULL;

-- +goose Down
DROP TABLE IF EXISTS payouts.payouts, payments.c2b_unmatched, payments.payments;
