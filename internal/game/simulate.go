package game

import (
	"errors"
	"math"

	"github.com/itsivali/lottery-platform/internal/game/fair"
)

// SimConfig describes the odds version to simulate and how much to play.
type SimConfig struct {
	Slots, LuckySlots int
	PriceKES          int64
	Tiers             map[int]int // plays in a day -> draw chances (before the lucky doubling)
	Days              int         // simulated player-days for each number of plays (1..DailyPlays)
}

// SimRow is the outcome for players who play the same number of times every day.
type SimRow struct {
	Plays         int     `json:"plays"`
	Days          int     `json:"days"`
	StakeKES      int64   `json:"stake_kes"`
	LuckyRate     float64 `json:"lucky_rate"`
	LuckyRateWant float64 `json:"lucky_rate_theory"`
	Chances       float64 `json:"mean_chances"`
	ChancesWant   float64 `json:"mean_chances_theory"`
	KESPerChance  float64 `json:"kes_per_chance"`
}

// SimReport is a simulation of the odds version: how often a day is lucky and what a draw chance costs. Return to
// player needs the prize structures (phase 4); until then the stakes are all the operator's revenue.
type SimReport struct {
	Slots      int      `json:"slots"`
	LuckySlots int      `json:"lucky_slots"`
	PriceKES   int64    `json:"price_kes"`
	Spins      int64    `json:"spins"`
	ChiSquare  float64  `json:"slot_chi_square"`
	Rows       []SimRow `json:"by_plays_per_day"`
	RTP        string   `json:"rtp"`
}

// Simulate plays the configured odds with real fair spins (fresh random seeds) and compares with the theory.
func Simulate(c SimConfig) (SimReport, error) {
	if c.Slots < 2 || c.LuckySlots < 1 || c.LuckySlots >= c.Slots || c.PriceKES <= 0 || c.Days < 1 {
		return SimReport{}, errors.New("game: bad simulation config")
	}
	seed, err := fair.NewServerSeed()
	if err != nil {
		return SimReport{}, err
	}
	rep := SimReport{Slots: c.Slots, LuckySlots: c.LuckySlots, PriceKES: c.PriceKES,
		RTP: "not available until draws and prize structures exist (phase 4): no stake is returned to players yet"}
	counts := make([]int64, c.Slots)
	var nonce int64
	pLucky := float64(c.LuckySlots) / float64(c.Slots)
	for k := 1; k <= DailyPlays; k++ {
		row := SimRow{Plays: k, Days: c.Days, StakeKES: int64(k) * c.PriceKES}
		var luckyDays, chances int
		for range c.Days {
			lucky := false
			for range k {
				slot, err := fair.Slot(seed, "simulation", nonce, c.Slots)
				if err != nil {
					return SimReport{}, err
				}
				nonce++
				counts[slot]++
				lucky = lucky || slot < c.LuckySlots
			}
			ch := c.Tiers[k]
			if lucky {
				luckyDays++
				ch *= 2
			}
			chances += ch
		}
		row.LuckyRate = float64(luckyDays) / float64(c.Days)
		row.LuckyRateWant = 1 - math.Pow(1-pLucky, float64(k))
		row.Chances = float64(chances) / float64(c.Days)
		row.ChancesWant = float64(c.Tiers[k]) * (1 + row.LuckyRateWant)
		if row.Chances > 0 {
			row.KESPerChance = float64(row.StakeKES) / row.Chances
		}
		rep.Rows = append(rep.Rows, row)
	}
	rep.Spins = nonce
	want := float64(nonce) / float64(c.Slots)
	for _, n := range counts {
		d := float64(n) - want
		rep.ChiSquare += d * d / want
	}
	return rep, nil
}
