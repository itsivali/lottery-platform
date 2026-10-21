// Package db opens Postgres pools and runs migrations.
package db

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"

	"github.com/itsivali/lottery-platform/db/migrations"
)

// Open creates a pgx pool. It does not require the database to be reachable yet.
func Open(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("db: open pool: %w", err)
	}
	return pool, nil
}

// RiverSchema is the schema holding River's job tables.
const RiverSchema = "river"

// MigrateUp applies all pending migrations: goose first (which creates the river schema), then River's own.
func MigrateUp(ctx context.Context, dsn string) error {
	if err := withGoose(dsn, func(sdb *sql.DB) error { return goose.UpContext(ctx, sdb, ".") }); err != nil {
		return err
	}
	if err := migrateRiver(ctx, dsn, rivermigrate.DirectionUp); err != nil {
		return err
	}
	return grantRiver(ctx, dsn)
}

// migrationLockID is the Postgres advisory lock that serialises migrations across instances.
const migrationLockID int64 = 727272001

// MigrateUpExclusive is MigrateUp guarded by a session advisory lock, so several instances booting at once (a
// rolling deploy) migrate one at a time and the later ones find nothing left to do.
func MigrateUpExclusive(ctx context.Context, dsn string) error {
	lock, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("db: connect for migration lock: %w", err)
	}
	defer func() { _ = lock.Close(ctx) }()
	if _, err := lock.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLockID); err != nil {
		return fmt.Errorf("db: take migration lock: %w", err)
	}
	defer func() { _, _ = lock.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", migrationLockID) }()
	return MigrateUp(ctx, dsn)
}

// MigrateDown rolls back every migration (used by tests and local resets only).
func MigrateDown(ctx context.Context, dsn string) error {
	if err := migrateRiver(ctx, dsn, rivermigrate.DirectionDown); err != nil {
		return err
	}
	return withGoose(dsn, func(sdb *sql.DB) error { return goose.DownToContext(ctx, sdb, ".", 0) })
}

func migrateRiver(ctx context.Context, dsn string, dir rivermigrate.Direction) error {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("db: open pool for river migrations: %w", err)
	}
	defer pool.Close()
	m, err := rivermigrate.New(riverpgxv5.New(pool), &rivermigrate.Config{Schema: RiverSchema})
	if err != nil {
		return err
	}
	opts := &rivermigrate.MigrateOpts{}
	if dir == rivermigrate.DirectionDown {
		opts.TargetVersion = -1
	}
	_, err = m.Migrate(ctx, dir, opts)
	return err
}

// grantRiver gives the application roles access to River's tables: the api enqueues (insert-only client), the worker runs jobs.
// Without the roles (single-role mode, see migration 00002) there is nothing to grant.
func grantRiver(ctx context.Context, dsn string) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(ctx) }()
	_, err = conn.Exec(ctx, `DO $$
DECLARE t record;
BEGIN
  IF (SELECT count(*) FROM pg_roles WHERE rolname IN ('app_migrator','app_api','app_worker','app_readonly')) < 4
     OR NOT pg_has_role(current_user, 'app_migrator', 'SET') THEN
    RAISE WARNING 'application roles unavailable: skipping river grants (SINGLE-ROLE MODE)';
    RETURN;
  END IF;
  GRANT USAGE ON SCHEMA river TO app_api, app_worker, app_readonly;
  -- River inserts with an upsert, so the api needs UPDATE as well; it still cannot delete or run jobs.
  GRANT SELECT, INSERT, UPDATE ON ALL TABLES IN SCHEMA river TO app_api;
  GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA river TO app_worker;
  GRANT SELECT ON ALL TABLES IN SCHEMA river TO app_readonly;
  GRANT USAGE ON ALL SEQUENCES IN SCHEMA river TO app_api, app_worker;
  ALTER SCHEMA river OWNER TO app_migrator;
  FOR t IN SELECT tablename FROM pg_tables WHERE schemaname = 'river' LOOP
    EXECUTE format('ALTER TABLE river.%I OWNER TO app_migrator', t.tablename);
  END LOOP;
END $$`)
	return err
}

func withGoose(dsn string, fn func(*sql.DB) error) error {
	cfg, err := pgxpoolConfig(dsn)
	if err != nil {
		return err
	}
	sdb := stdlib.OpenDB(*cfg)
	defer func() { _ = sdb.Close() }()
	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	return fn(sdb)
}
