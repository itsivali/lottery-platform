-- +goose Up
-- Module schemas, application roles and shared helpers. Privileges are granted in 00008_grants.sql.
CREATE SCHEMA IF NOT EXISTS platform;
CREATE SCHEMA IF NOT EXISTS identity;
CREATE SCHEMA IF NOT EXISTS kyc;
CREATE SCHEMA IF NOT EXISTS rg;
CREATE SCHEMA IF NOT EXISTS wallet;
CREATE SCHEMA IF NOT EXISTS payments;
CREATE SCHEMA IF NOT EXISTS ledger;
CREATE SCHEMA IF NOT EXISTS tickets;
CREATE SCHEMA IF NOT EXISTS game;
CREATE SCHEMA IF NOT EXISTS draws;
CREATE SCHEMA IF NOT EXISTS payouts;
CREATE SCHEMA IF NOT EXISTS audit;

-- Roles are cluster-wide and survive a rollback. NOLOGIN: login roles / passwords are provisioned at deploy time
-- and granted membership in these (never in this repository).
--
-- Role separation needs a role allowed to create roles. A managed Postgres (for example Render) gives the
-- application a database owner that is not a superuser and may not be able to; in that case this block only warns
-- and the schema runs in single-role mode. The invariants that matter most (append-only, ledger balance, wallet
-- reconciliation, play cap, one-time draw seed) are triggers and constraints and hold for every role regardless.
-- +goose StatementBegin
DO $$
DECLARE r text;
BEGIN
  FOREACH r IN ARRAY ARRAY['app_migrator','app_api','app_worker','app_readonly'] LOOP
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r) THEN
      EXECUTE format('CREATE ROLE %I NOLOGIN', r);
    END IF;
  END LOOP;
  -- the migrating role must be able to hand ownership to app_migrator (needs the SET option on PostgreSQL 16+)
  EXECUTE format('GRANT app_migrator TO %I WITH SET TRUE', current_user);
  EXECUTE format('GRANT CREATE ON DATABASE %I TO app_migrator', current_database());
EXCEPTION WHEN insufficient_privilege THEN
  RAISE WARNING 'cannot create or grant application roles as %: running in SINGLE-ROLE MODE (no privilege separation between api, worker and readonly)', current_user;
END $$;
-- +goose StatementEnd

-- UUIDv7 (RFC 9562): 48-bit unix-ms timestamp, version 7, random remainder. Time-ordered primary keys.
-- +goose StatementBegin
CREATE FUNCTION platform.uuid_v7() RETURNS uuid
LANGUAGE sql VOLATILE PARALLEL SAFE AS $$
  SELECT encode(
    set_bit(set_bit(
      overlay(uuid_send(gen_random_uuid())
        placing substring(int8send((extract(epoch FROM clock_timestamp()) * 1000)::bigint) FROM 3)
        FROM 1 FOR 6),
      52, 1), 53, 1),
    'hex')::uuid
$$;
-- +goose StatementEnd

-- Append-only guard: fires for every role, including table owners and superusers.
-- +goose StatementBegin
CREATE FUNCTION platform.forbid_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION '% on %.% is forbidden: table is append-only (post a compensating entry)',
    TG_OP, TG_TABLE_SCHEMA, TG_TABLE_NAME USING ERRCODE = 'restrict_violation';
END $$;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION IF EXISTS platform.forbid_mutation();
DROP FUNCTION IF EXISTS platform.uuid_v7();
DROP SCHEMA IF EXISTS audit CASCADE;
DROP SCHEMA IF EXISTS payouts CASCADE;
DROP SCHEMA IF EXISTS draws CASCADE;
DROP SCHEMA IF EXISTS game CASCADE;
DROP SCHEMA IF EXISTS tickets CASCADE;
DROP SCHEMA IF EXISTS ledger CASCADE;
DROP SCHEMA IF EXISTS payments CASCADE;
DROP SCHEMA IF EXISTS wallet CASCADE;
DROP SCHEMA IF EXISTS rg CASCADE;
DROP SCHEMA IF EXISTS kyc CASCADE;
DROP SCHEMA IF EXISTS identity CASCADE;
DROP SCHEMA IF EXISTS platform CASCADE;
