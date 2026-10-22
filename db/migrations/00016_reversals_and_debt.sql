-- +goose Up
-- Phase 2 slice 4: reversals of credited top-ups and per-player debt recovery.
ALTER TABLE wallet.wallet_transactions DROP CONSTRAINT wallet_transactions_kind_check;
ALTER TABLE wallet.wallet_transactions ADD CONSTRAINT wallet_transactions_kind_check
  CHECK (kind IN ('deposit','deposit_excise','stake','withdrawal','prize','refund','adjustment','reversal','debt_recovery'));

ALTER TABLE payments.payments DROP CONSTRAINT payments_status_check;
ALTER TABLE payments.payments ADD CONSTRAINT payments_status_check
  CHECK (status IN ('initiated','stk_sent','callback_received','verified','credited','failed','cancelled','expired',
                    'unconfirmed','reversed'));
ALTER TABLE payments.payments
  ADD COLUMN reversed_at     timestamptz,
  ADD COLUMN reversal_reason text,
  ADD CONSTRAINT payments_reversed_has_reason CHECK (status <> 'reversed' OR (reversed_at IS NOT NULL AND reversal_reason IS NOT NULL)),
  -- only a credited payment can be reversed, so a reversed one keeps its confirmed amount and receipt
  ADD CONSTRAINT payments_reversed_was_credited CHECK (status <> 'reversed' OR (amount_confirmed_kes IS NOT NULL AND mpesa_receipt IS NOT NULL));

-- a player's debt is the balance of their PLAYER_DEBT entries
CREATE INDEX ledger_entries_account_user_idx ON ledger.ledger_entries (account_id, user_id) WHERE user_id IS NOT NULL;

-- Daily internal reconciliation results (payments vs ledger vs wallets); append-only.
CREATE TABLE payments.reconciliation_runs (
  id      uuid PRIMARY KEY DEFAULT platform.uuid_v7(),
  ran_at  timestamptz NOT NULL DEFAULT now(),
  ok      boolean NOT NULL,
  report  jsonb NOT NULL
);
CREATE TRIGGER reconciliation_runs_append_only BEFORE UPDATE OR DELETE ON payments.reconciliation_runs
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();

-- +goose StatementBegin
DO $$
BEGIN
  IF (SELECT count(*) FROM pg_roles WHERE rolname IN ('app_migrator','app_api','app_worker','app_readonly')) < 4
     OR NOT pg_has_role(current_user, 'app_migrator', 'SET') THEN
    RAISE WARNING 'application roles unavailable: skipping grants (SINGLE-ROLE MODE)';
    RETURN;
  END IF;
  GRANT SELECT, INSERT ON payments.reconciliation_runs TO app_api, app_worker;
  GRANT SELECT ON payments.reconciliation_runs TO app_readonly;
  ALTER TABLE payments.reconciliation_runs OWNER TO app_migrator;
END $$;
-- +goose StatementEnd

-- +goose Down
DROP TABLE payments.reconciliation_runs;
DROP INDEX ledger.ledger_entries_account_user_idx;
ALTER TABLE payments.payments DROP CONSTRAINT payments_reversed_was_credited, DROP CONSTRAINT payments_reversed_has_reason,
  DROP COLUMN reversal_reason, DROP COLUMN reversed_at;
UPDATE payments.payments SET status = 'credited' WHERE status = 'reversed';
ALTER TABLE payments.payments DROP CONSTRAINT payments_status_check;
ALTER TABLE payments.payments ADD CONSTRAINT payments_status_check
  CHECK (status IN ('initiated','stk_sent','callback_received','verified','credited','failed','cancelled','expired','unconfirmed'));
ALTER TABLE wallet.wallet_transactions DROP CONSTRAINT wallet_transactions_kind_check;
ALTER TABLE wallet.wallet_transactions ADD CONSTRAINT wallet_transactions_kind_check
  CHECK (kind IN ('deposit','deposit_excise','stake','withdrawal','prize','refund','adjustment','reversal'));
