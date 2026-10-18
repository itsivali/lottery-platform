// Package testpg starts a throwaway Postgres container for integration tests.
package testpg

import (
	"context"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Start returns a connection string for a fresh Postgres 16 and a stop func (also registered via t.Cleanup).
func Start(t *testing.T) (dsn string, stop func()) {
	t.Helper()
	ctx := context.Background()
	c, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("lottery"),
		postgres.WithUsername("lottery"),
		postgres.WithPassword("lottery"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(60*time.Second)),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	stop = func() { _ = testcontainers.TerminateContainer(c) }
	t.Cleanup(stop)
	dsn, err = c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}
	return dsn, stop
}
