package payments

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/itsivali/lottery-platform/internal/ledger"
	"github.com/itsivali/lottery-platform/internal/payments/store"
)

// Report is one internal reconciliation run. Mismatches break an invariant between payments, ledger and wallets and
// must never happen (a bug or tampering: investigate). Attention lists what needs an operator: unconfirmed and overdue
// top-ups, orphan callbacks, players in debt, open suspense items, and paybill payments Daraja has not confirmed.
// Neither replaces matching against the M-Pesa statement (later).
type Report struct {
	RanAt      time.Time           `json:"ran_at"`
	OK         bool                `json:"ok"`
	Mismatches map[string][]string `json:"mismatches"`
	Attention  map[string][]string `json:"attention"`
	Totals     map[string]int64    `json:"totals"`
}

// orphanWindow is how far back orphan callbacks are listed; c2bOverdue is when a paybill payment still being verified
// needs a look.
const (
	orphanWindow = 30 * 24 * time.Hour
	c2bOverdue   = time.Hour
)

// Reconcile checks payments against the ledger and wallets, stores the run and returns it.
func (s *Service) Reconcile(ctx context.Context) (Report, error) {
	r := Report{Mismatches: map[string][]string{}, Attention: map[string][]string{}, Totals: map[string]int64{}}
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
			"credited_without_deposit": q.CreditedWithoutDeposit,
			"deposit_without_credit":   q.DepositWithoutCredit,
			"deposit_amount_mismatch":  q.DepositAmountMismatch,
			"reversal_mismatch":        q.ReversalMismatch,
			"suspense_without_journal": q.SuspenseWithoutJournal,
		} {
			ids, err := check(ctx)
			if err != nil {
				return err
			}
			if len(ids) > 0 {
				r.Mismatches[name] = ids
			}
		}

		clearing, err := q.AccountBalance(ctx, ledger.MpesaClearing)
		if err != nil {
			return err
		}
		credited, err := q.CreditedTotal(ctx)
		if err != nil {
			return err
		}
		liability, err := q.AccountBalance(ctx, ledger.WalletLiability)
		if err != nil {
			return err
		}
		wallets, err := q.WalletsTotal(ctx)
		if err != nil {
			return err
		}
		suspenseItems, err := q.SuspenseOpenTotal(ctx)
		if err != nil {
			return err
		}
		suspense, err := q.AccountBalance(ctx, ledger.Suspense)
		if err != nil {
			return err
		}
		r.Totals["clearing_kes"], r.Totals["credited_kes"], r.Totals["wallets_kes"] = clearing, credited, wallets
		r.Totals["suspense_kes"] = suspenseItems
		// M-Pesa holds what was credited plus what waits in suspense
		if clearing != credited+suspenseItems {
			r.Mismatches["clearing_vs_credited"] = []string{"MPESA_CLEARING " + strconv.FormatInt(clearing, 10) + " != credited " +
				strconv.FormatInt(credited, 10) + " + suspense " + strconv.FormatInt(suspenseItems, 10)}
		}
		if -suspense != suspenseItems { // the suspense account is credit-normal
			r.Mismatches["suspense_vs_items"] = []string{"SUSPENSE " + strconv.FormatInt(-suspense, 10) + " != open items " + strconv.FormatInt(suspenseItems, 10)}
		}
		if -liability != wallets { // the liability is credit-normal
			r.Mismatches["liability_vs_wallets"] = []string{"WALLET_LIABILITY " + strconv.FormatInt(-liability, 10) + " != wallets " + strconv.FormatInt(wallets, 10)}
		}

		if r.Attention["unconfirmed"], err = q.UnconfirmedPayments(ctx); err != nil {
			return err
		}
		if r.Attention["overdue_pending"], err = q.OverduePending(ctx, now.Add(-s.cfg.Expiry)); err != nil {
			return err
		}
		if r.Attention["orphan_callbacks"], err = q.RecentOrphanCallbacks(ctx, now.Add(-orphanWindow)); err != nil {
			return err
		}
		if r.Attention["suspense_open"], err = q.OpenSuspense(ctx); err != nil {
			return err
		}
		if r.Attention["c2b_unverified"], err = q.UnverifiedC2B(ctx); err != nil {
			return err
		}
		if r.Attention["c2b_overdue"], err = q.OverdueC2B(ctx, now.Add(-c2bOverdue)); err != nil {
			return err
		}
		if r.Attention["c2b_conflict_after_credit"], err = q.ConflictsAfterCredit(ctx); err != nil {
			return err
		}
		debtors, err := q.PlayersInDebt(ctx)
		if err != nil {
			return err
		}
		for _, d := range debtors {
			r.Attention["players_in_debt"] = append(r.Attention["players_in_debt"], d.UserID)
			r.Totals["debt_kes"] += d.Debt
		}
		for k, v := range r.Attention {
			if len(v) == 0 {
				delete(r.Attention, k)
			}
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
	switch {
	case !r.OK:
		slog.ErrorContext(ctx, "payments reconciliation found mismatches", "mismatches", r.Mismatches, "totals", r.Totals)
	case len(r.Attention) > 0:
		slog.WarnContext(ctx, "payments reconciliation: items need an operator", "attention", r.Attention, "totals", r.Totals)
	default:
		slog.InfoContext(ctx, "payments reconciliation clean", "totals", r.Totals)
	}
	return r, nil
}
