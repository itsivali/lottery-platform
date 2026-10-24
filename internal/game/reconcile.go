package game

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/itsivali/lottery-platform/internal/game/fair"
	"github.com/itsivali/lottery-platform/internal/game/store"
)

// Report is one game reconciliation run. Mismatches break an invariant between tickets, the ledger, spins and seed
// chains; they must never happen (a bug or tampering: investigate).
type Report struct {
	RanAt      time.Time           `json:"ran_at"`
	OK         bool                `json:"ok"`
	Mismatches map[string][]string `json:"mismatches"`
	Totals     map[string]int64    `json:"totals"`
}

// RecheckWindow is how far back revealed chains have every spin recomputed (each run overlaps the previous one).
const RecheckWindow = 48 * time.Hour

// Reconcile checks the game's books, stores the run and returns it.
func (s *Service) Reconcile(ctx context.Context) (Report, error) {
	r := Report{Mismatches: map[string][]string{}, Totals: map[string]int64{}}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		// one consistent snapshot for every check
		if _, err := tx.Exec(ctx, "SET TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY"); err != nil {
			return err
		}
		now, err := s.clk.Now(ctx, tx)
		if err != nil {
			return err
		}
		r.RanAt = now
		for name, check := range map[string]func(context.Context) ([]string, error){
			"ticket_without_stake":   q.TicketsWithoutStake,
			"stake_without_ticket":   q.StakesWithoutTicket,
			"stake_amount_mismatch":  q.StakeAmountMismatch,
			"ticket_without_spin":    q.TicketsWithoutSpin,
			"day_tally_mismatch":     q.DayTallyMismatch,
			"nonce_gap":              q.NonceGaps,
			"revealed_seed_mismatch": q.RevealedSeedMismatch,
		} {
			ids, err := check(ctx)
			if err != nil {
				return err
			}
			if len(ids) > 0 {
				r.Mismatches[name] = ids
			}
		}
		if err := recomputeSpins(ctx, q, now.Add(-RecheckWindow), &r); err != nil {
			return err
		}
		revenue, err := q.StakesRevenue(ctx)
		if err != nil {
			return err
		}
		paid, err := q.PaidTicketsTotal(ctx)
		if err != nil {
			return err
		}
		funded, err := q.PoolFundingTotal(ctx)
		if err != nil {
			return err
		}
		r.Totals["stakes_revenue_kes"], r.Totals["paid_tickets"], r.Totals["paid_tickets_kes"] = revenue, paid.Tickets, paid.Kes
		r.Totals["prize_pools_funded_kes"] = funded
		// approved draws move their pools out of stakes revenue (phase 4)
		if revenue != paid.Kes-funded {
			r.Mismatches["revenue_vs_tickets"] = []string{"STAKES_REVENUE " + strconv.FormatInt(revenue, 10) + " != paid tickets " +
				strconv.FormatInt(paid.Kes, 10) + " - prize pools funded " + strconv.FormatInt(funded, 10)}
		}
		r.OK = len(r.Mismatches) == 0
		return nil
	})
	if err != nil {
		return Report{}, err
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return Report{}, err
	}
	if err := store.New(s.pool).InsertReconciliationRun(ctx, store.InsertReconciliationRunParams{RanAt: r.RanAt, Ok: r.OK, Report: raw}); err != nil {
		return Report{}, err
	}
	if !r.OK {
		slog.ErrorContext(ctx, "game reconciliation found mismatches", "mismatches", r.Mismatches, "totals", r.Totals)
	} else {
		slog.InfoContext(ctx, "game reconciliation clean", "totals", r.Totals)
	}
	return r, nil
}

// recomputeSpins re-derives every spin of chains revealed since `since` from the published seed: the stored slot and
// lucky flag must be what the specification gives.
func recomputeSpins(ctx context.Context, q *store.Queries, since time.Time, r *Report) error {
	spins, err := q.RevealedSpins(ctx, since)
	if err != nil {
		return err
	}
	for _, sp := range spins {
		var seed []byte
		if sp.RevealedSeed != nil {
			seed, _ = hex.DecodeString(*sp.RevealedSeed)
		}
		slot, err := fair.Slot(seed, sp.ClientSeed, sp.Nonce, int(sp.SlotCount))
		if err != nil || slot != int(sp.Slot) || sp.Lucky != (slot < int(sp.LuckySlots)) {
			r.Mismatches["spin_not_reproducible"] = append(r.Mismatches["spin_not_reproducible"], sp.TicketID)
		}
	}
	r.Totals["spins_recomputed"] = int64(len(spins))
	return nil
}
