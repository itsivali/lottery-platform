// Package clock is the only place business code obtains the current time.
//
// Business time (play_date, cutoffs, draw periods, limits, holds) is read once per transaction through a
// Clock and passed to SQL as a parameter. Production uses the transaction's Postgres now(); tests inject a
// fixed or stepped clock. Calling time.Now() for a business decision is rejected by lint.
package clock

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	// Distroless images carry no zoneinfo; embed it so Africa/Nairobi always resolves.
	_ "time/tzdata"
)

// Nairobi is Africa/Nairobi (EAT, UTC+3, no daylight saving).
var Nairobi = mustLoad("Africa/Nairobi")

func mustLoad(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic("clock: " + err.Error())
	}
	return loc
}

// InNairobi expresses t in Africa/Nairobi without changing the instant.
func InNairobi(t time.Time) time.Time { return t.In(Nairobi) }

// Querier is the minimal slice of pgx needed to read database time.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Clock returns the business time for a transaction.
type Clock interface {
	Now(ctx context.Context, q Querier) (time.Time, error)
}

// Postgres reads now() from the database: stable for the life of the transaction.
type Postgres struct{}

// Now implements Clock.
func (Postgres) Now(ctx context.Context, q Querier) (time.Time, error) {
	var t time.Time
	if err := q.QueryRow(ctx, "SELECT now()").Scan(&t); err != nil {
		return time.Time{}, err
	}
	return t, nil
}

// Fixed always returns the same instant.
type Fixed struct{ at time.Time }

// NewFixed returns a Clock frozen at at.
func NewFixed(at time.Time) *Fixed { return &Fixed{at: at} }

// Now implements Clock.
func (f *Fixed) Now(context.Context, Querier) (time.Time, error) { return f.at, nil }

// Stepped returns start, then start+step, start+2*step, ... on successive reads.
type Stepped struct {
	mu   sync.Mutex
	next time.Time
	step time.Duration
}

// NewStepped returns a Clock that advances by step on every read.
func NewStepped(start time.Time, step time.Duration) *Stepped {
	return &Stepped{next: start, step: step}
}

// Now implements Clock.
func (s *Stepped) Now(context.Context, Querier) (time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.next
	s.next = s.next.Add(s.step)
	return t, nil
}

// Manual is a Clock tests move explicitly with Advance or Set.
type Manual struct {
	mu  sync.Mutex
	now time.Time
}

// NewManual returns a Manual clock reading at.
func NewManual(at time.Time) *Manual { return &Manual{now: at} }

// Now implements Clock.
func (m *Manual) Now(context.Context, Querier) (time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.now, nil
}

// Advance moves the clock forward by d.
func (m *Manual) Advance(d time.Duration) {
	m.mu.Lock()
	m.now = m.now.Add(d)
	m.mu.Unlock()
}

// Set jumps the clock to at.
func (m *Manual) Set(at time.Time) {
	m.mu.Lock()
	m.now = at
	m.mu.Unlock()
}
