package game_test

import (
	"math"
	"testing"

	"github.com/itsivali/lottery-platform/internal/game"
)

func TestSimulate_MatchesTheTheory(t *testing.T) {
	rep, err := game.Simulate(game.SimConfig{Slots: 12, LuckySlots: 1, PriceKES: 100, Tiers: map[int]int{1: 1, 2: 2, 3: 3, 4: 5}, Days: 20_000})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Rows) != game.DailyPlays || rep.Spins != 20_000*10 {
		t.Fatalf("report = %+v", rep)
	}
	for _, r := range rep.Rows {
		// 20k days: the lucky rate is within 5 standard errors of the theory (a fair generator fails ~1 in 3 million)
		se := math.Sqrt(r.LuckyRateWant * (1 - r.LuckyRateWant) / float64(r.Days))
		if math.Abs(r.LuckyRate-r.LuckyRateWant) > 5*se || r.StakeKES != int64(r.Plays)*100 {
			t.Errorf("row %+v", r)
		}
		if math.Abs(r.Chances-r.ChancesWant) > 0.1*r.ChancesWant || r.KESPerChance <= 0 {
			t.Errorf("chances %+v", r)
		}
	}
	if rep.ChiSquare > 31.26 { // 11 degrees of freedom, p = 0.001
		t.Errorf("slots not uniform: chi-square %.2f", rep.ChiSquare)
	}
	if math.Abs(rep.Rows[0].LuckyRateWant-1.0/12) > 1e-12 {
		t.Errorf("one play: theory %v", rep.Rows[0].LuckyRateWant)
	}
}

func TestSimulate_RefusesABadConfig(t *testing.T) {
	for _, c := range []game.SimConfig{
		{Slots: 1, LuckySlots: 1, PriceKES: 100, Days: 1},
		{Slots: 12, LuckySlots: 12, PriceKES: 100, Days: 1},
		{Slots: 12, LuckySlots: 0, PriceKES: 100, Days: 1},
		{Slots: 12, LuckySlots: 1, PriceKES: 0, Days: 1},
		{Slots: 12, LuckySlots: 1, PriceKES: 100, Days: 0},
	} {
		if _, err := game.Simulate(c); err == nil {
			t.Errorf("accepted %+v", c)
		}
	}
}
