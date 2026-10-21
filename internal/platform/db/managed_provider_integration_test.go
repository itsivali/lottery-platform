//go:build integration

package db_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/itsivali/lottery-platform/internal/platform/db"
	"github.com/itsivali/lottery-platform/internal/platform/testpg"
)

// A managed Postgres (Render) gives the application a database owner that is NOT a superuser, and the platform does
// not document whether it may create roles. Migrations must succeed either way; role separation is hardening that
// degrades loudly, while the in-database invariants (triggers, constraints) never depend on it.
func managedUser(t *testing.T, createRole bool) (dsn string, admin *pgx.Conn) {
	t.Helper()
	ctx := context.Background()
	// roles are cluster-wide, so each scenario needs its own fresh server (the shared one already has them)
	base, _ := testpg.Start(t)
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close(ctx) })
	suffix := fmt.Sprintf("%d", dbCounter())
	user, name := "render_user_"+suffix, "render_db_"+suffix
	attr := "LOGIN PASSWORD 'pw'"
	if createRole {
		attr += " CREATEROLE"
	}
	for _, sql := range []string{
		fmt.Sprintf("CREATE ROLE %s %s", user, attr),
		fmt.Sprintf("CREATE DATABASE %s OWNER %s", name, user),
	} {
		if _, err := admin.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", name))
	})
	return testpg.WithUser(testpg.WithDatabase(base, name), user, "pw"), admin
}

var counter int

func dbCounter() int { counter++; return counter }

func TestMigrations_RunAsANonSuperuserWithCreateRole(t *testing.T) {
	dsn, _ := managedUser(t, true)
	if err := db.MigrateUp(context.Background(), dsn); err != nil {
		t.Fatalf("migrate as a CREATEROLE non-superuser: %v", err)
	}
	assertOwners(t, dsn, "app_migrator")
}

func TestMigrations_RunAsANonSuperuserWithoutCreateRoleAndKeepEveryDatabaseInvariant(t *testing.T) {
	dsn, _ := managedUser(t, false)
	ctx := context.Background()
	if err := db.MigrateUp(ctx, dsn); err != nil {
		t.Fatalf("migrate as a plain non-superuser: %v", err)
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, `INSERT INTO audit.audit_log (actor_type, action, entity_type) VALUES ('system','x','y')`); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{`UPDATE audit.audit_log SET action = 'z'`, `DELETE FROM audit.audit_log`} {
		if _, err := conn.Exec(ctx, s); sqlState(err) != codeRestrict {
			t.Errorf("%s -> %v: append-only must hold without role separation", s, err)
		}
	}
	// rolling back and forward again must also work in this mode
	if err := db.MigrateDown(ctx, dsn); err != nil {
		t.Fatalf("down: %v", err)
	}
	if err := db.MigrateUp(ctx, dsn); err != nil {
		t.Fatalf("up again: %v", err)
	}
}

func assertOwners(t *testing.T, dsn, want string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	var bad int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM pg_tables WHERE schemaname IN ('identity','wallet','ledger','audit') AND tableowner <> $1`, want).Scan(&bad); err != nil {
		t.Fatal(err)
	}
	if bad != 0 {
		t.Fatalf("%d tables not owned by %s", bad, want)
	}
}
