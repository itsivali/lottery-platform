//go:build integration

package audit_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/itsivali/lottery-platform/internal/audit"
	"github.com/itsivali/lottery-platform/internal/platform/testpg"
)

func TestRecord_WritesInsideTheCallersTransaction(t *testing.T) {
	pool := testpg.Pool(t, "app_api")
	ctx := context.Background()
	w := audit.NewWriter()
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		return w.Record(ctx, tx, audit.Event{
			ActorType: audit.Staff, ActorID: "alice", Action: "config.odds.update",
			EntityType: "odds_config", EntityID: "abc", Details: map[string]any{"slots": 12, "reason": "test"},
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	var action, actor string
	var slots float64
	if err := pool.QueryRow(ctx, `SELECT action, actor_id, (details->>'slots')::float8 FROM audit.audit_log`).Scan(&action, &actor, &slots); err != nil {
		t.Fatal(err)
	}
	if action != "config.odds.update" || actor != "alice" || slots != 12 {
		t.Fatalf("row = %q %q %v", action, actor, slots)
	}
}

func TestRecord_RollsBackWithTheActionItDescribes(t *testing.T) {
	pool := testpg.Pool(t, "app_api")
	ctx := context.Background()
	boom := errors.New("business step failed")
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := audit.NewWriter().Record(ctx, tx, audit.Event{ActorType: audit.System, Action: "x", EntityType: "y"}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM audit.audit_log`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d audit rows survived a rolled-back transaction", n)
	}
}

func TestRecord_RejectsMalformedEvents(t *testing.T) {
	pool := testpg.Pool(t, "app_api")
	ctx := context.Background()
	bad := []audit.Event{
		{ActorType: "robot", Action: "x", EntityType: "y"},
		{ActorType: audit.System, EntityType: "y"},
		{ActorType: audit.System, Action: "x"},
		{ActorType: audit.Staff, Action: "x", EntityType: "y"}, // staff events must name the staff member
		{ActorType: audit.Player, Action: "x", EntityType: "y"},
	}
	for i, ev := range bad {
		err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error { return audit.NewWriter().Record(ctx, tx, ev) })
		if err == nil {
			t.Errorf("event %d accepted: %+v", i, ev)
		}
	}
}

func TestRecord_EncodesNilDetailsAsAnEmptyObject(t *testing.T) {
	pool := testpg.Pool(t, "app_api")
	ctx := context.Background()
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		return audit.NewWriter().Record(ctx, tx, audit.Event{ActorType: audit.System, Action: "tick", EntityType: "job"})
	}); err != nil {
		t.Fatal(err)
	}
	var d string
	_ = pool.QueryRow(ctx, `SELECT details::text FROM audit.audit_log`).Scan(&d)
	if d != "{}" {
		t.Fatalf("details = %q, want {}", d)
	}
}
