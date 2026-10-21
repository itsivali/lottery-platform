package testpg

import (
	"context"
	"fmt"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/itsivali/lottery-platform/internal/platform/db"
)

var (
	sharedOnce sync.Once
	sharedErr  error
	sharedBase string // DSN to the maintenance database of the shared server
	dbCounter  atomic.Int64
)

// Migrated returns the DSN of a private, fully migrated database cloned from a template.
// One Postgres container serves the whole test process; the container is reaped by testcontainers' Ryuk.
func Migrated(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	sharedOnce.Do(func() { sharedErr = startShared(ctx) })
	if sharedErr != nil {
		t.Fatalf("shared postgres: %v", sharedErr)
	}
	name := fmt.Sprintf("t_%d_%d", time.Now().UnixNano(), dbCounter.Add(1)) //nolint:forbidigo // test-only unique name
	admin, err := pgx.Connect(ctx, sharedBase)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = admin.Close(ctx) }()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name+" TEMPLATE lottery_template"); err != nil {
		t.Fatalf("clone template: %v", err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(ctx, sharedBase)
		if err != nil {
			return
		}
		defer func() { _ = c.Close(ctx) }()
		_, _ = c.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	return withDB(sharedBase, name)
}

func startShared(ctx context.Context) error {
	c, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("postgres"),
		postgres.WithUsername("lottery"),
		postgres.WithPassword("lottery"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(60*time.Second)),
	)
	if err != nil {
		return err
	}
	base, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return err
	}
	sharedBase = base
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		return err
	}
	defer func() { _ = admin.Close(ctx) }()
	if _, err := admin.Exec(ctx, "CREATE DATABASE lottery_template"); err != nil {
		return err
	}
	if err := db.MigrateUp(ctx, withDB(base, "lottery_template")); err != nil {
		return fmt.Errorf("migrate template: %w", err)
	}
	return nil
}

func withDB(dsn, name string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		panic(err)
	}
	u.Path = "/" + name
	return u.String()
}

// Pool returns a pgx pool on a fresh migrated database whose connections run as the given application role
// (for example "app_api"), so tests exercise the real least-privilege grants.
func Pool(t *testing.T, role string) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(Migrated(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 25
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, "SET ROLE "+pgx.Identifier{role}.Sanitize())
		return err
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}
