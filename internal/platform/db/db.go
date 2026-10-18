// Package db opens Postgres pools and runs migrations.
package db

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

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

// MigrateUp applies all pending migrations.
func MigrateUp(ctx context.Context, dsn string) error {
	return withGoose(dsn, func(sdb *sql.DB) error { return goose.UpContext(ctx, sdb, ".") })
}

// MigrateDown rolls back every migration (used by tests and local resets only).
func MigrateDown(ctx context.Context, dsn string) error {
	return withGoose(dsn, func(sdb *sql.DB) error { return goose.DownToContext(ctx, sdb, ".", 0) })
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
