-- +goose Up
-- Phase 3 slice 3: the daily game reconciliation (stakes ⇔ ledger ⇔ tickets ⇔ spins ⇔ seed chains). Runs are kept,
-- append-only, like the payments reconciliation.
CREATE TABLE game.reconciliation_runs (
  id      uuid PRIMARY KEY DEFAULT platform.uuid_v7(),
  ran_at  timestamptz NOT NULL DEFAULT now(),
  ok      boolean NOT NULL,
  report  jsonb NOT NULL
);
CREATE TRIGGER game_reconciliation_runs_append_only BEFORE UPDATE OR DELETE ON game.reconciliation_runs
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();

-- +goose StatementBegin
DO $$
BEGIN
  IF (SELECT count(*) FROM pg_roles WHERE rolname IN ('app_migrator','app_api','app_worker','app_readonly')) < 4
     OR NOT pg_has_role(current_user, 'app_migrator', 'SET') THEN
    RAISE WARNING 'application roles unavailable: skipping grants (SINGLE-ROLE MODE)';
    RETURN;
  END IF;
  GRANT SELECT, INSERT ON game.reconciliation_runs TO app_api, app_worker;
  GRANT SELECT ON game.reconciliation_runs TO app_readonly;
  ALTER TABLE game.reconciliation_runs OWNER TO app_migrator;
END $$;
-- +goose StatementEnd

-- +goose Down
DROP TABLE game.reconciliation_runs;
