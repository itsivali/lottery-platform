package rg_test

import (
	"errors"
	"testing"
	"time"

	"github.com/itsivali/lottery-platform/internal/platform/clock"
	"github.com/itsivali/lottery-platform/internal/rg"
)

func TestWindowStart_UsesNairobiCalendarBoundaries(t *testing.T) {
	nb := func(y int, m time.Month, d, h, mi int) time.Time {
		return time.Date(y, m, d, h, mi, 0, 0, clock.Nairobi)
	}
	cases := []struct {
		name   string
		period rg.Period
		now    time.Time
		want   time.Time
	}{
		{"daily mid-day", rg.Daily, nb(2026, 10, 21, 15, 0), nb(2026, 10, 21, 0, 0)},
		{"daily just after midnight EAT (still previous UTC day)", rg.Daily, time.Date(2026, 10, 20, 21, 30, 0, 0, time.UTC), nb(2026, 10, 21, 0, 0)},
		{"weekly on Wednesday", rg.Weekly, nb(2026, 10, 21, 15, 0), nb(2026, 10, 19, 0, 0)},
		{"weekly on Monday", rg.Weekly, nb(2026, 10, 19, 0, 1), nb(2026, 10, 19, 0, 0)},
		{"weekly on Sunday", rg.Weekly, nb(2026, 10, 25, 23, 59), nb(2026, 10, 19, 0, 0)},
		{"monthly mid-month", rg.Monthly, nb(2026, 10, 21, 15, 0), nb(2026, 10, 1, 0, 0)},
		{"monthly rolls at EAT midnight", rg.Monthly, time.Date(2026, 10, 31, 21, 30, 0, 0, time.UTC), nb(2026, 11, 1, 0, 0)},
	}
	for _, c := range cases {
		got, err := rg.WindowStart(c.period, c.now)
		if err != nil || !got.Equal(c.want) {
			t.Errorf("%s: got %v, %v; want %v", c.name, got, err, c.want)
		}
	}
	if _, err := rg.WindowStart("yearly", time.Now()); err == nil { //nolint:forbidigo // value irrelevant, period is invalid
		t.Error("unknown period accepted")
	}
}

func TestCheckDeposit(t *testing.T) {
	limits := map[rg.Period]int64{rg.Daily: 1000, rg.Weekly: 3000}
	spent := map[rg.Period]int64{rg.Daily: 100, rg.Weekly: 2500, rg.Monthly: 99999} // monthly has no limit
	cases := []struct {
		name       string
		amount     int64
		wantPeriod rg.Period // "" = allowed
		remaining  int64
	}{
		{"within every limit", 100, "", 0},
		{"exactly the weekly remainder", 500, "", 0},
		{"over the weekly limit", 501, rg.Weekly, 500},
		{"over the daily limit is reported first", 901, rg.Daily, 900},
	}
	for _, c := range cases {
		err := rg.CheckDeposit(limits, spent, c.amount)
		if c.wantPeriod == "" {
			if err != nil {
				t.Errorf("%s: %v", c.name, err)
			}
			continue
		}
		var le *rg.LimitError
		if !errors.As(err, &le) || le.Period != c.wantPeriod || le.Remaining != c.remaining {
			t.Errorf("%s: err = %v, want %s limit with %d remaining", c.name, err, c.wantPeriod, c.remaining)
		}
	}
	if err := rg.CheckDeposit(limits, spent, 0); err == nil {
		t.Error("zero deposit accepted")
	}
	if err := rg.CheckDeposit(nil, nil, 5000); err != nil {
		t.Errorf("no limits set: %v", err)
	}
}
