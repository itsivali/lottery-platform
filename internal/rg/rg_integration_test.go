//go:build integration

package rg_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/itsivali/lottery-platform/internal/audit"
	"github.com/itsivali/lottery-platform/internal/platform/clock"
	"github.com/itsivali/lottery-platform/internal/platform/testpg"
	"github.com/itsivali/lottery-platform/internal/rg"
)

type env struct {
	pool *pgxpool.Pool
	clk  *clock.Manual
	svc  *rg.Service
	user string
}

var seq int

func setup(t *testing.T) *env {
	t.Helper()
	pool := testpg.Pool(t, "app_api")
	e := &env{pool: pool, clk: clock.NewManual(time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC))}
	e.svc = rg.NewService(pool, e.clk, audit.NewWriter(), rg.Config{CoolingOff: 24 * time.Hour})
	seq++
	if err := pool.QueryRow(context.Background(), `INSERT INTO identity.users (msisdn) VALUES ($1) RETURNING id::text`, fmt.Sprintf("2547%08d", seq)).Scan(&e.user); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) limit(t *testing.T, p rg.Period) rg.LimitState {
	t.Helper()
	m, err := e.svc.Limits(context.Background(), e.user)
	if err != nil {
		t.Fatal(err)
	}
	return m[p]
}

func (e *env) restricted(t *testing.T) error {
	t.Helper()
	return pgx.BeginFunc(context.Background(), e.pool, func(tx pgx.Tx) error { return e.svc.AssertNotRestricted(context.Background(), tx, e.user) })
}

func TestLimits_FirstLimitAndDecreasesApplyImmediately(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	if _, err := e.svc.SetLimit(ctx, e.user, rg.Daily, 5000); err != nil {
		t.Fatal(err)
	}
	if st := e.limit(t, rg.Daily); st.Current == nil || st.Current.AmountKES != 5000 || st.Pending != nil {
		t.Fatalf("after first limit: %+v", st)
	}
	e.clk.Advance(time.Minute)
	if _, err := e.svc.SetLimit(ctx, e.user, rg.Daily, 2000); err != nil {
		t.Fatal(err)
	}
	if st := e.limit(t, rg.Daily); st.Current.AmountKES != 2000 {
		t.Fatalf("decrease not immediate: %+v", st)
	}
}

func TestLimits_IncreaseWaitsForTheCoolingOffAndShowsAsPending(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	_, _ = e.svc.SetLimit(ctx, e.user, rg.Weekly, 2000)
	e.clk.Advance(time.Hour)
	l, err := e.svc.SetLimit(ctx, e.user, rg.Weekly, 9000)
	if err != nil {
		t.Fatal(err)
	}
	if want := now(e.clk).Add(24 * time.Hour); !l.EffectiveFrom.Equal(want) {
		t.Fatalf("EffectiveFrom = %v, want %v", l.EffectiveFrom, want)
	}
	st := e.limit(t, rg.Weekly)
	if st.Current.AmountKES != 2000 || st.Pending == nil || st.Pending.AmountKES != 9000 {
		t.Fatalf("during cooling-off: %+v", st)
	}
	e.clk.Advance(23*time.Hour + 59*time.Minute)
	if st := e.limit(t, rg.Weekly); st.Current.AmountKES != 2000 {
		t.Fatalf("increase applied early: %+v", st)
	}
	e.clk.Advance(time.Minute)
	if st := e.limit(t, rg.Weekly); st.Current.AmountKES != 9000 || st.Pending != nil {
		t.Fatalf("increase not applied after cooling-off: %+v", st)
	}
}

func TestLimits_ALaterDecreaseCancelsAPendingIncrease(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	_, _ = e.svc.SetLimit(ctx, e.user, rg.Monthly, 4000)
	e.clk.Advance(time.Minute)
	_, _ = e.svc.SetLimit(ctx, e.user, rg.Monthly, 8000) // pending
	e.clk.Advance(time.Minute)
	_, _ = e.svc.SetLimit(ctx, e.user, rg.Monthly, 1000) // lowers now, must also void the pending raise
	if st := e.limit(t, rg.Monthly); st.Current.AmountKES != 1000 || st.Pending != nil {
		t.Fatalf("after decrease: %+v", st)
	}
	e.clk.Advance(48 * time.Hour)
	if st := e.limit(t, rg.Monthly); st.Current.AmountKES != 1000 {
		t.Fatalf("cancelled increase came back: %+v", st)
	}
}

func TestLimits_RejectInvalidInput(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	for _, amt := range []int64{0, -1, 1_000_000_001} {
		if _, err := e.svc.SetLimit(ctx, e.user, rg.Daily, amt); !errors.Is(err, rg.ErrInvalidLimit) {
			t.Errorf("amount %d err = %v", amt, err)
		}
	}
	if _, err := e.svc.SetLimit(ctx, e.user, "hourly", 100); !errors.Is(err, rg.ErrInvalidLimit) {
		t.Errorf("bad period err = %v", err)
	}
}

func TestTimeout_BlocksUntilItEndsAndCannotBeShortened(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	if err := e.restricted(t); err != nil {
		t.Fatalf("unrestricted account blocked: %v", err)
	}
	if err := e.svc.Timeout(ctx, e.user, "7d"); err != nil {
		t.Fatal(err)
	}
	var re *rg.RestrictedError
	if err := e.restricted(t); !errors.As(err, &re) || re.Kind != rg.KindTimeout || re.Until == nil {
		t.Fatalf("err = %v", err)
	}
	if err := e.svc.Timeout(ctx, e.user, "24h"); err != nil { // a shorter one must not release the longer
		t.Fatal(err)
	}
	e.clk.Advance(25 * time.Hour)
	if err := e.restricted(t); err == nil {
		t.Fatal("a later, shorter time-out released the 7-day one")
	}
	e.clk.Advance(6 * 24 * time.Hour)
	if err := e.restricted(t); err != nil {
		t.Fatalf("still blocked after the time-out ended: %v", err)
	}
	if err := e.svc.Timeout(ctx, e.user, "1h"); !errors.Is(err, rg.ErrInvalidDuration) {
		t.Fatalf("invalid duration err = %v", err)
	}
}

func TestSelfExclusion_BlocksImmediatelyForTheChosenTermAndPermanentNeverEnds(t *testing.T) {
	cases := []struct {
		term      string
		addMonths int
		permanent bool
	}{{"6m", 6, false}, {"1y", 12, false}, {"permanent", 0, true}}
	for _, c := range cases {
		e := setup(t)
		ctx := context.Background()
		if err := e.svc.SelfExclude(ctx, e.user, c.term); err != nil {
			t.Fatal(err)
		}
		var re *rg.RestrictedError
		if err := e.restricted(t); !errors.As(err, &re) || re.Kind != rg.KindSelfExclusion {
			t.Fatalf("%s: not blocked immediately: %v", c.term, err)
		}
		if c.permanent {
			if re.Until != nil {
				t.Errorf("permanent exclusion has an end: %v", re.Until)
			}
			e.clk.Advance(100 * 365 * 24 * time.Hour)
			if err := e.restricted(t); err == nil {
				t.Error("permanent exclusion expired")
			}
			continue
		}
		start := now(e.clk)
		end := start.AddDate(0, c.addMonths, 0)
		if !re.Until.Equal(end) {
			t.Errorf("%s: Until = %v, want %v", c.term, re.Until, end)
		}
		e.clk.Set(end.Add(-time.Second))
		if err := e.restricted(t); err == nil {
			t.Errorf("%s: released early", c.term)
		}
		e.clk.Set(end)
		if err := e.restricted(t); err != nil {
			t.Errorf("%s: still blocked at the end: %v", c.term, err)
		}
	}
	e := setup(t)
	if err := e.svc.SelfExclude(context.Background(), e.user, "2w"); !errors.Is(err, rg.ErrInvalidDuration) {
		t.Fatalf("invalid term err = %v", err)
	}
}

func TestSelfExclusion_IsAuditedAndRestrictionsAreAppendOnly(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	_ = e.svc.SelfExclude(ctx, e.user, "6m")
	var n int
	_ = e.pool.QueryRow(ctx, `SELECT count(*) FROM audit.audit_log WHERE action = 'rg.self_exclusion' AND actor_id = $1`, e.user).Scan(&n)
	if n != 1 {
		t.Fatalf("%d audit rows", n)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE rg.restrictions SET ends_at = now()`); err == nil {
		t.Fatal("app role shortened a restriction")
	}
	if _, err := e.pool.Exec(ctx, `DELETE FROM rg.restrictions`); err == nil {
		t.Fatal("app role deleted a restriction")
	}
}

func TestRestrictions_AreScopedToTheUser(t *testing.T) {
	a := setup(t)
	_ = a.svc.SelfExclude(context.Background(), a.user, "permanent")
	var other string
	seq++
	_ = a.pool.QueryRow(context.Background(), `INSERT INTO identity.users (msisdn) VALUES ($1) RETURNING id::text`, fmt.Sprintf("2547%08d", seq)).Scan(&other)
	err := pgx.BeginFunc(context.Background(), a.pool, func(tx pgx.Tx) error { return a.svc.AssertNotRestricted(context.Background(), tx, other) })
	if err != nil {
		t.Fatalf("another player was blocked: %v", err)
	}
}

func now(c *clock.Manual) time.Time { n, _ := c.Now(context.Background(), nil); return n }

func TestLimits_ASecondIncreaseReplacesThePendingOne(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	_, _ = e.svc.SetLimit(ctx, e.user, rg.Daily, 1000)
	e.clk.Advance(time.Minute)
	_, _ = e.svc.SetLimit(ctx, e.user, rg.Daily, 9000)
	e.clk.Advance(time.Hour)
	_, _ = e.svc.SetLimit(ctx, e.user, rg.Daily, 3000) // still an increase over 1000, but lower than the pending 9000
	st := e.limit(t, rg.Daily)
	if st.Pending == nil || st.Pending.AmountKES != 3000 {
		t.Fatalf("pending = %+v, want only the 3000 request", st.Pending)
	}
	e.clk.Advance(25 * time.Hour)
	if st := e.limit(t, rg.Daily); st.Current.AmountKES != 3000 {
		t.Fatalf("the cancelled 9000 raise took effect: %+v", st)
	}
}
