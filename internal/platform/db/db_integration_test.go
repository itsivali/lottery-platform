//go:build integration

package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/itsivali/lottery-platform/internal/platform/db"
	"github.com/itsivali/lottery-platform/internal/platform/testpg"
)

func TestPool_PingSucceedsWhenPostgresIsUp(t *testing.T) {
	dsn, _ := testpg.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestPool_PingFailsWhenPostgresIsDown(t *testing.T) {
	dsn, stop := testpg.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	stop()
	if err := pool.Ping(ctx); err == nil {
		t.Fatal("Ping succeeded after Postgres stopped")
	}
}

func TestMigrations_UpDownUp(t *testing.T) {
	dsn, _ := testpg.Start(t)
	ctx := context.Background()
	for _, step := range []struct {
		name string
		fn   func(context.Context, string) error
	}{{"up", db.MigrateUp}, {"down", db.MigrateDown}, {"up again", db.MigrateUp}} {
		if err := step.fn(ctx, dsn); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
	}
}
