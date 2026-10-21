// Package rg implements responsible-gaming controls: deposit limits with a cooling-off on increases, time-outs and
// self-exclusion. Restrictions are append-only and can never be shortened.
package rg

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/itsivali/lottery-platform/internal/audit"
	"github.com/itsivali/lottery-platform/internal/platform/clock"
	"github.com/itsivali/lottery-platform/internal/rg/store"
)

// Period of a deposit limit.
type Period string

// Limit periods.
const (
	Daily   Period = "daily"
	Weekly  Period = "weekly"
	Monthly Period = "monthly"
)

var periods = []Period{Daily, Weekly, Monthly}

// Restriction kinds.
const (
	KindTimeout       = "timeout"
	KindSelfExclusion = "self_exclusion"
)

// maxLimitKES bounds a limit so arithmetic on it can never overflow.
const maxLimitKES int64 = 1_000_000_000

// Errors.
var (
	ErrInvalidLimit    = errors.New("rg: invalid limit")
	ErrInvalidDuration = errors.New("rg: invalid duration")
)

// RestrictedError says why a player may not top up or play. Until is nil for permanent exclusion.
type RestrictedError struct {
	Kind  string
	Until *time.Time
}

func (e *RestrictedError) Error() string { return "rg: account is restricted (" + e.Kind + ")" }

// LimitError says a deposit would exceed a limit.
type LimitError struct {
	Period    Period
	Limit     int64
	Remaining int64
}

func (e *LimitError) Error() string {
	return "rg: deposit would exceed the " + string(e.Period) + " limit"
}

// Limit is one deposit limit version.
type Limit struct {
	Period        Period
	AmountKES     int64
	EffectiveFrom time.Time
}

// LimitState is the limit in force plus an increase waiting out its cooling-off.
type LimitState struct {
	Current *Limit
	Pending *Limit
}

// Config tunes the service.
type Config struct {
	CoolingOff time.Duration // delay before a limit increase applies (DECISION, default 24h)
}

// Service manages limits and restrictions.
type Service struct {
	pool  *pgxpool.Pool
	clk   clock.Clock
	audit *audit.Writer
	cfg   Config
}

// NewService returns a Service.
func NewService(pool *pgxpool.Pool, clk clock.Clock, a *audit.Writer, cfg Config) *Service {
	return &Service{pool: pool, clk: clk, audit: a, cfg: cfg}
}

func validPeriod(p Period) bool { return p == Daily || p == Weekly || p == Monthly }

// SetLimit lowers a limit immediately or raises it after the cooling-off. Any pending raise is voided.
func (s *Service) SetLimit(ctx context.Context, userID string, p Period, amountKES int64) (Limit, error) {
	if !validPeriod(p) || amountKES <= 0 || amountKES > maxLimitKES {
		return Limit{}, ErrInvalidLimit
	}
	var out Limit
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		now, err := s.clk.Now(ctx, tx)
		if err != nil {
			return err
		}
		q := store.New(tx)
		state, err := s.state(ctx, q, userID, now)
		if err != nil {
			return err
		}
		if err := q.CancelPendingLimits(ctx, store.CancelPendingLimitsParams{At: &now, UserID: userID, Period: string(p)}); err != nil {
			return err
		}
		eff := now
		if c := state[p].Current; c != nil && amountKES > c.AmountKES {
			eff = now.Add(s.cfg.CoolingOff)
		}
		if err := q.UpsertLimit(ctx, store.UpsertLimitParams{UserID: userID, Period: string(p), AmountKes: amountKES, EffectiveFrom: eff, RequestedAt: now}); err != nil {
			return err
		}
		out = Limit{Period: p, AmountKES: amountKES, EffectiveFrom: eff}
		return s.audit.Record(ctx, tx, audit.Event{ActorType: audit.Player, ActorID: userID, Action: "rg.limit.set", EntityType: "user", EntityID: userID,
			Details: map[string]any{"period": string(p), "amount_kes": amountKES, "effective_from": eff}})
	})
	return out, err
}

func (s *Service) state(ctx context.Context, q *store.Queries, userID string, now time.Time) (map[Period]LimitState, error) {
	rows, err := q.ListLimits(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := map[Period]LimitState{}
	for _, r := range rows { // ordered by period, effective_from
		l := Limit{Period: Period(r.Period), AmountKES: r.AmountKes, EffectiveFrom: r.EffectiveFrom}
		st := out[l.Period]
		if !l.EffectiveFrom.After(now) {
			c := l
			st.Current = &c
		} else if st.Pending == nil {
			pnd := l
			st.Pending = &pnd
		}
		out[l.Period] = st
	}
	return out, nil
}

// Limits returns the limits in force and any pending raises for every period that has one.
func (s *Service) Limits(ctx context.Context, userID string) (map[Period]LimitState, error) {
	now, err := s.clk.Now(ctx, s.pool)
	if err != nil {
		return nil, err
	}
	return s.state(ctx, store.New(s.pool), userID, now)
}

var timeoutDurations = map[string]time.Duration{"24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour, "30d": 30 * 24 * time.Hour}

// Timeout blocks the player for 24h, 7d or 30d.
func (s *Service) Timeout(ctx context.Context, userID, duration string) error {
	d, ok := timeoutDurations[duration]
	if !ok {
		return ErrInvalidDuration
	}
	return s.restrict(ctx, userID, KindTimeout, "rg.timeout", duration, func(now time.Time) *time.Time {
		end := now.Add(d)
		return &end
	})
}

// SelfExclude blocks the player for 6 months, 1 year or permanently.
func (s *Service) SelfExclude(ctx context.Context, userID, term string) error {
	var end func(time.Time) *time.Time
	switch term {
	case "6m":
		end = func(n time.Time) *time.Time { e := n.AddDate(0, 6, 0); return &e }
	case "1y":
		end = func(n time.Time) *time.Time { e := n.AddDate(1, 0, 0); return &e }
	case "permanent":
		end = func(time.Time) *time.Time { return nil }
	default:
		return ErrInvalidDuration
	}
	return s.restrict(ctx, userID, KindSelfExclusion, "rg.self_exclusion", term, end)
}

func (s *Service) restrict(ctx context.Context, userID, kind, action, term string, end func(time.Time) *time.Time) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		now, err := s.clk.Now(ctx, tx)
		if err != nil {
			return err
		}
		if err := store.New(tx).InsertRestriction(ctx, store.InsertRestrictionParams{UserID: userID, Kind: kind, StartsAt: now, EndsAt: end(now)}); err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, audit.Event{ActorType: audit.Player, ActorID: userID, Action: action, EntityType: "user", EntityID: userID,
			Details: map[string]any{"term": term}})
	})
}

// AssertNotRestricted returns a *RestrictedError if the player is in a time-out or self-excluded. Callers invoke it in
// the same transaction as the debit or top-up (invariant 12).
func (s *Service) AssertNotRestricted(ctx context.Context, tx pgx.Tx, userID string) error {
	now, err := s.clk.Now(ctx, tx)
	if err != nil {
		return err
	}
	rows, err := store.New(tx).ActiveRestrictions(ctx, store.ActiveRestrictionsParams{UserID: userID, StartsAt: now})
	if err != nil {
		return err
	}
	var worst *RestrictedError
	for _, r := range rows {
		if r.EndsAt == nil { // permanent beats everything
			return &RestrictedError{Kind: r.Kind}
		}
		if worst == nil || r.EndsAt.After(*worst.Until) {
			end := *r.EndsAt
			worst = &RestrictedError{Kind: r.Kind, Until: &end}
		}
	}
	if worst != nil {
		return worst
	}
	return nil
}

// WindowStart returns the start of the limit window containing now, on Africa/Nairobi calendar boundaries
// (weeks start on Monday).
func WindowStart(p Period, now time.Time) (time.Time, error) {
	n := clock.InNairobi(now)
	day := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, clock.Nairobi)
	switch p {
	case Daily:
		return day, nil
	case Weekly:
		return day.AddDate(0, 0, -((int(n.Weekday()) + 6) % 7)), nil
	case Monthly:
		return time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, clock.Nairobi), nil
	}
	return time.Time{}, ErrInvalidLimit
}

// CheckDeposit refuses a deposit that would push any period past its limit. spent is what the player has already
// deposited in each current window (supplied by the wallet), limits the amounts in force.
func CheckDeposit(limits, spent map[Period]int64, amount int64) error {
	if amount <= 0 {
		return errors.New("rg: deposit must be positive")
	}
	for _, p := range periods {
		lim, ok := limits[p]
		if !ok {
			continue
		}
		if spent[p]+amount > lim {
			return &LimitError{Period: p, Limit: lim, Remaining: max(lim-spent[p], 0)}
		}
	}
	return nil
}
