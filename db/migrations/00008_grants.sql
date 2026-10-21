-- +goose Up
-- All privileges live here so they can be audited in one place.
-- Model: app_migrator owns every object; app_api / app_worker get DML but never DDL, TRUNCATE or DELETE (except where
-- listed); app_readonly can only SELECT. History tables get INSERT and SELECT only.

-- +goose StatementBegin
DO $$
DECLARE
  s text; t record;
BEGIN
  FOREACH s IN ARRAY ARRAY['platform','identity','kyc','rg','wallet','payments','ledger','tickets','game','draws','payouts','audit'] LOOP
    EXECUTE format('ALTER SCHEMA %I OWNER TO app_migrator', s);
    EXECUTE format('GRANT USAGE ON SCHEMA %I TO app_api, app_worker, app_readonly', s);
    EXECUTE format('REVOKE ALL ON ALL TABLES IN SCHEMA %I FROM PUBLIC', s);
    FOR t IN SELECT tablename FROM pg_tables WHERE schemaname = s LOOP
      EXECUTE format('ALTER TABLE %I.%I OWNER TO app_migrator', s, t.tablename);
      EXECUTE format('GRANT SELECT, INSERT, UPDATE ON %I.%I TO app_api, app_worker', s, t.tablename);
      EXECUTE format('GRANT SELECT ON %I.%I TO app_readonly', s, t.tablename);
    END LOOP;
    -- identity columns / sequences
    EXECUTE format('GRANT USAGE ON ALL SEQUENCES IN SCHEMA %I TO app_api, app_worker', s);
  END LOOP;
END $$;
-- +goose StatementEnd

-- Functions are owned by the migrator as well.
-- +goose StatementBegin
DO $$
DECLARE f record;
BEGIN
  FOR f IN SELECT p.oid::regprocedure AS sig FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
           WHERE n.nspname IN ('platform','ledger','wallet','draws') LOOP
    EXECUTE format('ALTER FUNCTION %s OWNER TO app_migrator', f.sig);
  END LOOP;
END $$;
-- +goose StatementEnd

-- Append-only history: no UPDATE, ever.
REVOKE UPDATE ON ledger.ledger_entries, ledger.ledger_journals, wallet.wallet_transactions, audit.audit_log,
  kyc.kyc_checks, rg.restrictions, game.spins, draws.draw_entries, draws.draw_results FROM app_api, app_worker;

-- Rows the application must be able to delete for retention only.
GRANT DELETE ON identity.phone_otps, platform.idempotency_keys TO app_worker;

-- kra_outbox: the worker may mark rows exported and nothing else; the api only appends.
REVOKE UPDATE ON audit.kra_outbox FROM app_api, app_worker;
GRANT UPDATE (exported_at) ON audit.kra_outbox TO app_worker;
REVOKE UPDATE ON audit.kra_outbox FROM app_api;

-- Config tables are written by the back-office through the api, and are versioned: no UPDATE of existing versions.
REVOKE UPDATE ON game.odds_config, game.chance_tiers, draws.prize_structures, ledger.tax_rules, ledger.ledger_accounts
  FROM app_api, app_worker;

-- Only the worker advances draws, payouts and payments state machines; the api may create but not change them.
REVOKE UPDATE ON draws.draws, draws.draw_signoffs, payouts.payouts FROM app_api;

-- +goose Down
-- Privileges disappear with the schemas dropped by 00002 down.
SELECT 1;
