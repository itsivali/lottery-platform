package clock_test

import (
	"context"
	"testing"
	"time"

	"github.com/itsivali/lottery-platform/internal/platform/clock"
)

func TestFixed_ReturnsTheSameInstantEveryTime(t *testing.T) {
	at := time.Date(2026, 10, 18, 20, 59, 59, 0, clock.Nairobi)
	c := clock.NewFixed(at)
	for range 3 {
		got, err := c.Now(context.Background(), nil)
		if err != nil || !got.Equal(at) {
			t.Fatalf("Now = %v, %v; want %v", got, err, at)
		}
	}
}

func TestStepped_AdvancesByStepOnEachRead(t *testing.T) {
	start := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	c := clock.NewStepped(start, time.Minute)
	first, _ := c.Now(context.Background(), nil)
	second, _ := c.Now(context.Background(), nil)
	if !first.Equal(start) || second.Sub(first) != time.Minute {
		t.Fatalf("got %v then %v", first, second)
	}
}

func TestNairobi_IsUTCPlusThreeWithoutDST(t *testing.T) {
	for _, m := range []time.Month{time.January, time.July} {
		_, off := time.Date(2026, m, 1, 12, 0, 0, 0, clock.Nairobi).Zone()
		if off != 3*3600 {
			t.Fatalf("month %v offset = %d, want 10800", m, off)
		}
	}
}

func TestInNairobi_ConvertsWithoutChangingTheInstant(t *testing.T) {
	utc := time.Date(2026, 10, 18, 18, 0, 0, 0, time.UTC) // 21:00 EAT
	got := clock.InNairobi(utc)
	if !got.Equal(utc) || got.Hour() != 21 {
		t.Fatalf("got %v", got)
	}
}

func TestManual_AdvancesOnlyWhenTold(t *testing.T) {
	start := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	c := clock.NewManual(start)
	a, _ := c.Now(context.Background(), nil)
	b, _ := c.Now(context.Background(), nil)
	if !a.Equal(start) || !b.Equal(start) {
		t.Fatalf("clock moved by itself: %v %v", a, b)
	}
	c.Advance(90 * time.Second)
	d, _ := c.Now(context.Background(), nil)
	if d.Sub(start) != 90*time.Second {
		t.Fatalf("after Advance: %v", d)
	}
	c.Set(start)
	if e, _ := c.Now(context.Background(), nil); !e.Equal(start) {
		t.Fatalf("after Set: %v", e)
	}
}
