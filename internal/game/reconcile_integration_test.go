//go:build integration

package game_test

import (
	"context"
	"fmt"
	"testing"
)

func (e *env) mustExec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatal(err)
	}
}

func TestReconcile_CleanAfterRealPlaysAndRotations(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	for i := range 3 {
		u := e.player(t, 1000)
		for j := range i + 1 {
			if _, err := e.svc.Play(ctx, u, fmt.Sprint("k", j)); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := e.svc.Rotate(ctx, u, ""); err != nil {
			t.Fatal(err)
		}
	}
	r, err := e.svc.Reconcile(ctx)
	if err != nil || !r.OK || len(r.Mismatches) != 0 {
		t.Fatalf("report %+v %v", r, err)
	}
	if r.Totals["paid_tickets"] != 6 || r.Totals["stakes_revenue_kes"] != 600 || r.Totals["spins_recomputed"] != 6 {
		t.Fatalf("totals %v", r.Totals)
	}
	var runs int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM game.reconciliation_runs WHERE ok`).Scan(&runs); err != nil || runs != 1 {
		t.Fatalf("stored runs %d %v", runs, err)
	}
}

func TestReconcile_CatchesEveryKindOfTampering(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	u := e.player(t, 1000)
	res, err := e.svc.Play(ctx, u, "a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Rotate(ctx, u, ""); err != nil {
		t.Fatal(err)
	}
	// a ticket sold outside the play path: no stake, no spin, revenue short
	e.mustExec(t, `INSERT INTO tickets.tickets (user_id, play_date, seq, price_kes, idempotency_key) VALUES ($1, '2026-01-01', 1, 100, 'forged')`, u)
	// a day that claims another play and luck it never had
	e.mustExec(t, `UPDATE tickets.player_days SET paid_plays = 2, lucky = NOT lucky WHERE user_id = $1`, u)
	// the new chain claims a spin it never made
	e.mustExec(t, `UPDATE game.seed_chains SET next_nonce = 3 WHERE user_id = $1 AND status = 'active'`, u)
	// the published seed is swapped for another one
	e.mustExec(t, `UPDATE game.seed_chains SET revealed_seed = repeat('ab', 32) WHERE user_id = $1 AND status = 'rotated'`, u)

	r, err := e.svc.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ticket_without_stake", "ticket_without_spin", "revenue_vs_tickets", "day_tally_mismatch",
		"nonce_gap", "revealed_seed_mismatch", "spin_not_reproducible"} {
		if len(r.Mismatches[want]) == 0 {
			t.Errorf("%s not caught: %v", want, r.Mismatches)
		}
	}
	if r.OK || r.Mismatches["spin_not_reproducible"][0] != res.Play.TicketID {
		t.Fatalf("report %+v", r)
	}
}
