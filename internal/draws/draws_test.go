package draws_test

import (
	"crypto/sha256"
	"testing"
	"time"

	"github.com/itsivali/lottery-platform/internal/draws"
)

func day(s string) time.Time { d, _ := time.Parse(time.DateOnly, s); return d }

func TestPeriod_EveryDrawTypeContainsTheDay(t *testing.T) {
	for _, tc := range []struct{ typ, on, start, end string }{
		{"daily", "2026-10-24", "2026-10-24", "2026-10-24"},
		{"weekly", "2026-10-24", "2026-10-19", "2026-10-25"}, // a Saturday: Monday to Sunday
		{"weekly", "2026-10-19", "2026-10-19", "2026-10-25"},
		{"weekly", "2026-10-25", "2026-10-19", "2026-10-25"},
		{"monthly", "2026-02-14", "2026-02-01", "2026-02-28"},
		{"quarterly", "2026-11-30", "2026-10-01", "2026-12-31"},
		{"quarterly", "2026-01-01", "2026-01-01", "2026-03-31"},
	} {
		s, e, err := draws.Period(tc.typ, day(tc.on))
		if err != nil || s.Format(time.DateOnly) != tc.start || e.Format(time.DateOnly) != tc.end {
			t.Errorf("%s %s: %s..%s %v", tc.typ, tc.on, s.Format(time.DateOnly), e.Format(time.DateOnly), err)
		}
	}
	if _, _, err := draws.Period("yearly", day("2026-01-01")); err == nil {
		t.Error("unknown type accepted")
	}
}

func TestFreezeTime_Is0005NairobiTheDayAfter(t *testing.T) {
	if got := draws.FreezeTime(day("2026-10-24")); !got.Equal(time.Date(2026, 10, 24, 21, 5, 0, 0, time.UTC)) {
		t.Fatalf("freeze %s", got)
	}
}

func TestPick_IsInRangeDeterministicAndUniform(t *testing.T) {
	key := sha256.Sum256([]byte("draw"))
	if draws.Pick(key[:], 0, 1) != 1 {
		t.Fatal("one entry")
	}
	first := draws.Pick(key[:], 7, 13)
	for range 3 {
		if draws.Pick(key[:], 7, 13) != first {
			t.Fatal("not deterministic")
		}
	}
	const n, picks = 7, 70_000
	counts := make([]int, n+1)
	for i := range picks {
		p := draws.Pick(key[:], i, n)
		if p < 1 || p > n {
			t.Fatalf("pick %d out of range", p)
		}
		counts[p]++
	}
	var x2 float64
	for _, c := range counts[1:] {
		d := float64(c) - picks/n
		x2 += d * d / (picks / n)
	}
	if x2 > 22.46 { // 6 degrees of freedom, p = 0.001
		t.Fatalf("not uniform: chi-square %.2f", x2)
	}
}
