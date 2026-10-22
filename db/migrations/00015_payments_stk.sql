-- +goose Up
-- STK top-ups (phase 2 slice 2).

-- 'unconfirmed': Daraja's status query says the customer paid but no callback (amount, receipt) arrived. Nothing is
-- credited without a receipt; reconciliation against the M-Pesa statement resolves these.
ALTER TABLE payments.payments DROP CONSTRAINT payments_status_check;
ALTER TABLE payments.payments ADD CONSTRAINT payments_status_check
  CHECK (status IN ('initiated','stk_sent','callback_received','verified','credited','failed','cancelled','expired','unconfirmed'));
ALTER TABLE payments.payments ADD COLUMN result_code integer;
DROP INDEX payments.payments_pending_idx;
CREATE INDEX payments_pending_idx ON payments.payments (created_at) WHERE status IN ('initiated','stk_sent','callback_received');

-- Callbacks whose checkout id matches no payment (a push whose acceptance we never recorded, or a forgery). Kept for
-- reconciliation; append-only.
CREATE TABLE payments.stk_orphan_callbacks (
  id                   uuid PRIMARY KEY DEFAULT platform.uuid_v7(),
  checkout_request_id  text NOT NULL,
  raw_payload          jsonb NOT NULL,
  received_at          timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER stk_orphan_callbacks_append_only BEFORE UPDATE OR DELETE ON payments.stk_orphan_callbacks
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();

-- Deposit excise version chosen by the owner on 2026-10-22 (Finance Act 2025, 5% of the amount deposited), to be
-- confirmed with a tax adviser before production. Without a version in force top-ups are refused.
INSERT INTO ledger.tax_rules (code, rate, point_of_taxation, rounding, effective_from, created_by)
VALUES ('deposit_excise', 0.05000, 'deposit', 'half_up', '2025-07-01', 'owner decision 2026-10-22');

-- +goose StatementBegin
DO $$
BEGIN
  IF (SELECT count(*) FROM pg_roles WHERE rolname IN ('app_migrator','app_api','app_worker','app_readonly')) < 4
     OR NOT pg_has_role(current_user, 'app_migrator', 'SET') THEN
    RAISE WARNING 'application roles unavailable: skipping grants (SINGLE-ROLE MODE)';
    RETURN;
  END IF;
  GRANT SELECT, INSERT ON payments.stk_orphan_callbacks TO app_api, app_worker;
  GRANT SELECT ON payments.stk_orphan_callbacks TO app_readonly;
  ALTER TABLE payments.stk_orphan_callbacks OWNER TO app_migrator;
END $$;
-- +goose StatementEnd

-- +goose Down
DELETE FROM ledger.tax_rules WHERE code = 'deposit_excise' AND effective_from = '2025-07-01';
DROP TABLE payments.stk_orphan_callbacks;
DROP INDEX payments.payments_pending_idx;
CREATE INDEX payments_pending_idx ON payments.payments (updated_at) WHERE status IN ('initiated','stk_sent','callback_received');
ALTER TABLE payments.payments DROP COLUMN result_code;
UPDATE payments.payments SET status = 'failed' WHERE status = 'unconfirmed';
ALTER TABLE payments.payments DROP CONSTRAINT payments_status_check;
ALTER TABLE payments.payments ADD CONSTRAINT payments_status_check
  CHECK (status IN ('initiated','stk_sent','callback_received','verified','credited','failed','cancelled','expired'));
