//go:build integration

package wallet_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"pgregory.net/rapid"

	"github.com/itsivali/lottery-platform/internal/ledger"
	"github.com/itsivali/lottery-platform/internal/platform/testpg"
	"github.com/itsivali/lottery-platform/internal/wallet"
)

var (
	msisdnSeq atomic.Int64
	idSeq     atomic.Int64
)

type env struct {
	pool   *pgxpool.Pool
	ledger *ledger.Store
	svc    *wallet.Service
}

func setup(t *testing.T) *env {
	t.Helper()
	pool := testpg.Pool(t, "app_api")
	ls := ledger.NewStore()
	return &env{pool: pool, ledger: ls, svc: wallet.NewService(ls)}
}

func (e *env) newUser(t testing.TB) string {
	t.Helper()
	var id string
	msisdn := fmt.Sprintf("2547%08d", msisdnSeq.Add(1))
	if err := e.pool.QueryRow(context.Background(),
		`INSERT INTO identity.users (msisdn) VALUES ($1) RETURNING id::text`, msisdn).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// newRef returns a fresh, unique event reference (uuid-shaped).
func newRef() string { return fmt.Sprintf("00000000-0000-7000-8000-%012d", idSeq.Add(1)) }

func (e *env) apply(ctx context.Context, p ledger.Posting, perr error) (int64, error) {
	if perr != nil {
		return 0, perr
	}
	var bal int64
	err := pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
		var err error
		bal, err = e.svc.Apply(ctx, tx, p)
		return err
	})
	return bal, err
}

func (e *env) balance(t testing.TB, user string) int64 {
	t.Helper()
	var b int64
	err := e.pool.QueryRow(context.Background(), `SELECT balance_kes FROM wallet.wallets WHERE user_id = $1`, user).Scan(&b)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal(err)
	}
	return b // 0 when the player has no wallet yet
}

func rule(t testing.TB, rate string) ledger.TaxRule {
	t.Helper()
	r, err := ledger.ParseRate(rate)
	if err != nil {
		t.Fatal(err)
	}
	return ledger.TaxRule{Code: "excise", Rate: r, Rounding: ledger.HalfUp}
}

func (e *env) deposit(ctx context.Context, user string, kes int64) (int64, error) {
	p, err := ledger.Deposit(newRef(), user, kes, ledger.TaxRule{Code: "excise", Rounding: ledger.HalfUp}, true)
	return e.apply(ctx, p, err)
}

func TestWallet_DepositCreditsTheConfirmedAmountNetOfExcise(t *testing.T) {
	e := setup(t)
	u := e.newUser(t)
	p, err := ledger.Deposit(newRef(), u, 1000, rule(t, "0.15"), true)
	bal, err := e.apply(context.Background(), p, err)
	if err != nil || bal != 850 {
		t.Fatalf("balance = %d, %v; want 850", bal, err)
	}
	if got := e.balance(t, u); got != 850 {
		t.Fatalf("stored balance = %d, want 850", got)
	}
}

func TestWallet_StakeWithInsufficientFundsIsRefusedAndLeavesNoTrace(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	u := e.newUser(t)
	if _, err := e.deposit(ctx, u, 50); err != nil {
		t.Fatal(err)
	}
	p, err := ledger.Stake(newRef(), u, 100)
	_, err = e.apply(ctx, p, err)
	if !errors.Is(err, wallet.ErrInsufficientFunds) {
		t.Fatalf("err = %v, want ErrInsufficientFunds", err)
	}
	if got := e.balance(t, u); got != 50 {
		t.Fatalf("balance = %d, want 50", got)
	}
	var n int
	_ = e.pool.QueryRow(ctx, `SELECT count(*) FROM ledger.ledger_journals WHERE kind = 'stake'`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d stake journals left behind by a refused stake", n)
	}
}

func TestWallet_PostingTheSameEventTwiceCreditsOnce(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	u := e.newUser(t)
	ref := newRef()
	post := func() error {
		p, err := ledger.Deposit(ref, u, 500, rule(t, "0"), true)
		_, err = e.apply(ctx, p, err)
		return err
	}
	if err := post(); err != nil {
		t.Fatal(err)
	}
	if err := post(); !errors.Is(err, wallet.ErrAlreadyPosted) {
		t.Fatalf("second post err = %v, want ErrAlreadyPosted", err)
	}
	if got := e.balance(t, u); got != 500 {
		t.Fatalf("balance = %d, want 500 (credited once)", got)
	}
}

func TestWallet_ConcurrentStakesNeverOverdraw(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	u := e.newUser(t)
	if _, err := e.deposit(ctx, u, 1000); err != nil {
		t.Fatal(err)
	}
	var ok, refused atomic.Int64
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := ledger.Stake(newRef(), u, 100)
			_, err = e.apply(ctx, p, err)
			switch {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, wallet.ErrInsufficientFunds):
				refused.Add(1)
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 10 || refused.Load() != 40 {
		t.Fatalf("ok=%d refused=%d, want 10/40", ok.Load(), refused.Load())
	}
	if got := e.balance(t, u); got != 0 {
		t.Fatalf("balance = %d, want 0", got)
	}
}

func TestWallet_ConcurrentStakesAndWithdrawalsNeverGoNegative(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	u := e.newUser(t)
	if _, err := e.deposit(ctx, u, 1000); err != nil {
		t.Fatal(err)
	}
	var spent atomic.Int64
	var wg sync.WaitGroup
	for i := range 60 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var p ledger.Posting
			var err error
			amount := int64(100)
			if i%2 == 0 {
				p, err = ledger.Stake(newRef(), u, amount)
			} else {
				amount = 70
				p, err = ledger.Withdrawal(newRef(), u, amount, rule(t, "0"))
			}
			if _, err = e.apply(ctx, p, err); err == nil {
				spent.Add(amount)
			} else if !errors.Is(err, wallet.ErrInsufficientFunds) {
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()
	got := e.balance(t, u)
	if got < 0 || got != 1000-spent.Load() {
		t.Fatalf("balance = %d, spent = %d", got, spent.Load())
	}
}

func TestWallet_DepositReversalBeyondBalanceRecordsPlayerDebt(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	u := e.newUser(t)
	payment := newRef()
	p, err := ledger.Deposit(payment, u, 1000, rule(t, "0"), true)
	if _, err = e.apply(ctx, p, err); err != nil {
		t.Fatal(err)
	}
	p, err = ledger.Stake(newRef(), u, 800)
	if _, err = e.apply(ctx, p, err); err != nil {
		t.Fatal(err)
	}
	err = pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
		_, bal, err := e.svc.ReverseDeposit(ctx, tx, payment, u, 1000, 0)
		if bal != 0 {
			t.Errorf("balance after reversal = %d, want 0", bal)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var debt int64
	_ = e.pool.QueryRow(ctx, `SELECT coalesce(sum(e.amount_kes),0) FROM ledger.ledger_entries e
		JOIN ledger.ledger_accounts a ON a.id = e.account_id WHERE a.code = 'PLAYER_DEBT' AND e.direction = 'D'`).Scan(&debt)
	if debt != 800 {
		t.Fatalf("player debt = %d, want 800", debt)
	}
}

// consistent asserts every ledger and wallet invariant after a step.
func (e *env) consistent(t interface{ Fatalf(string, ...any) }, users []string) {
	ctx := context.Background()
	var unbalanced int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM (SELECT journal_id FROM ledger.ledger_entries GROUP BY journal_id
		HAVING sum(CASE direction WHEN 'D' THEN amount_kes ELSE -amount_kes END) <> 0) x`).Scan(&unbalanced); err != nil {
		t.Fatalf("balance query: %v", err)
	}
	if unbalanced != 0 {
		t.Fatalf("%d unbalanced journals", unbalanced)
	}
	for _, u := range users {
		var bal, txSum, liability int64
		if err := e.pool.QueryRow(ctx, `SELECT coalesce(w.balance_kes,0),
			coalesce((SELECT sum(amount_kes) FROM wallet.wallet_transactions WHERE user_id = usr.id),0),
			coalesce((SELECT sum(CASE e.direction WHEN 'C' THEN e.amount_kes ELSE -e.amount_kes END) FROM ledger.ledger_entries e
			          JOIN ledger.ledger_accounts a ON a.id = e.account_id
			          WHERE a.code = 'WALLET_LIABILITY' AND e.user_id = usr.id),0)
			FROM identity.users usr LEFT JOIN wallet.wallets w ON w.user_id = usr.id WHERE usr.id = $1`, u).Scan(&bal, &txSum, &liability); err != nil {
			t.Fatalf("wallet query: %v", err)
		}
		if bal < 0 || bal != txSum || bal != liability {
			t.Fatalf("user %s: balance=%d txSum=%d ledgerLiability=%d", u, bal, txSum, liability)
		}
	}
}

func TestLedgerWallet_Property_RandomSequencesKeepEverythingBalanced(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	excise := rule(t, "0.16")
	wtax := rule(t, "0.20")

	rapid.Check(t, func(rt *rapid.T) {
		users := []string{e.newUser(t), e.newUser(t)}
		model := map[string]int64{}
		type deposit struct {
			ref    string
			user   string
			gross  int64
			excise int64 // deducted from the credit (0 when the operator absorbed it)
		}
		var deposits []deposit
		var withdrawals []struct {
			ref, user string
			amount    int64
		}
		var stakes []struct {
			ref, user string
			amount    int64
		}

		step := func(name string, build func() (ledger.Posting, error), after func(ledger.Posting)) {
			p, err := build()
			if err != nil {
				rt.Fatalf("%s: build: %v", name, err)
			}
			before := e.balance(t, p.UserID)
			_, err = e.apply(ctx, p, nil)
			switch {
			case err == nil:
				model[p.UserID] += p.WalletDelta()
				after(p)
			case errors.Is(err, wallet.ErrInsufficientFunds):
				if before+p.WalletDelta() >= 0 {
					rt.Fatalf("%s refused although balance %d covers %d", name, before, p.WalletDelta())
				}
			default:
				rt.Fatalf("%s: %v", name, err)
			}
			if got := e.balance(t, p.UserID); got != model[p.UserID] {
				rt.Fatalf("%s: balance %d, model %d", name, got, model[p.UserID])
			}
		}

		rt.Repeat(map[string]func(*rapid.T){
			"topup": func(rt *rapid.T) {
				u := rapid.SampledFrom(users).Draw(rt, "user")
				amt := rapid.Int64Range(10, 5000).Draw(rt, "amt")
				ref := newRef()
				deduct := rapid.Bool().Draw(rt, "deduct")
				step("topup", func() (ledger.Posting, error) { return ledger.Deposit(ref, u, amt, excise, deduct) },
					func(p ledger.Posting) { deposits = append(deposits, deposit{ref, u, amt, amt - p.WalletDelta()}) })
			},
			"play": func(rt *rapid.T) {
				u := rapid.SampledFrom(users).Draw(rt, "user")
				ref := newRef()
				step("play", func() (ledger.Posting, error) { return ledger.Stake(ref, u, 100) },
					func(ledger.Posting) {
						stakes = append(stakes, struct {
							ref, user string
							amount    int64
						}{ref, u, 100})
					})
			},
			"refund": func(rt *rapid.T) {
				if len(stakes) == 0 {
					return // nothing to act on yet
				}
				i := rapid.IntRange(0, len(stakes)-1).Draw(rt, "i")
				s := stakes[i]
				stakes = append(stakes[:i], stakes[i+1:]...)
				step("refund", func() (ledger.Posting, error) { return ledger.StakeRefund(s.ref, s.user, s.amount) }, func(ledger.Posting) {})
			},
			"withdraw": func(rt *rapid.T) {
				u := rapid.SampledFrom(users).Draw(rt, "user")
				amt := rapid.Int64Range(100, 3000).Draw(rt, "amt")
				ref := newRef()
				step("withdraw", func() (ledger.Posting, error) { return ledger.Withdrawal(ref, u, amt, wtax) },
					func(ledger.Posting) {
						withdrawals = append(withdrawals, struct {
							ref, user string
							amount    int64
						}{ref, u, amt})
					})
			},
			"withdrawal-fails": func(rt *rapid.T) {
				if len(withdrawals) == 0 {
					return // nothing to act on yet
				}
				i := rapid.IntRange(0, len(withdrawals)-1).Draw(rt, "i")
				w := withdrawals[i]
				withdrawals = append(withdrawals[:i], withdrawals[i+1:]...)
				step("withdrawal-fails", func() (ledger.Posting, error) { return ledger.WithdrawalFailed(w.ref, w.user, w.amount, wtax) }, func(ledger.Posting) {})
			},
			"reversal": func(rt *rapid.T) {
				if len(deposits) == 0 {
					return // nothing to act on yet
				}
				i := rapid.IntRange(0, len(deposits)-1).Draw(rt, "i")
				d := deposits[i]
				deposits = append(deposits[:i], deposits[i+1:]...)
				var delta int64
				err := pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
					p, _, err := e.svc.ReverseDeposit(ctx, tx, d.ref, d.user, d.gross, d.excise)
					delta = p.WalletDelta()
					return err
				})
				if err != nil {
					rt.Fatalf("reversal: %v", err)
				}
				model[d.user] += delta
				if got := e.balance(t, d.user); got != model[d.user] {
					rt.Fatalf("reversal: balance %d, model %d", got, model[d.user])
				}
			},
			"prize-payout": func(rt *rapid.T) {
				amt := rapid.Int64Range(100, 100000).Draw(rt, "amt")
				ref := newRef()
				p, err := ledger.PrizePayout(ref, "weekly", amt, wtax)
				if err != nil {
					rt.Fatalf("prize payout: %v", err)
				}
				// prize payouts bypass the wallet and go straight to the ledger
				if err := pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
					_, err := e.ledger.Post(ctx, tx, p.Journal)
					return err
				}); err != nil {
					rt.Fatalf("prize payout: %v", err)
				}
			},
			"": func(rt *rapid.T) { e.consistent(rt, users) },
		})
	})
}

func TestWallet_BalanceIsZeroForAPlayerWithoutAWalletAndTracksPostings(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	u := e.newUser(t)
	read := func() int64 {
		var b int64
		if err := pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
			var err error
			b, err = e.svc.Balance(ctx, tx, u)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return b
	}
	if got := read(); got != 0 {
		t.Fatalf("balance before any wallet = %d", got)
	}
	if _, err := e.deposit(ctx, u, 300); err != nil {
		t.Fatal(err)
	}
	if got := read(); got != 300 {
		t.Fatalf("balance = %d, want 300", got)
	}
}

func TestWallet_ApplyRefusesPostingsWithoutAUserOrWithABrokenJournal(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	p, _ := ledger.Stake(newRef(), "00000000-0000-7000-8000-0000000000aa", 100)
	noUser := p
	noUser.UserID = ""
	if _, err := e.apply(ctx, noUser, nil); err == nil {
		t.Error("posting without a user accepted")
	}
	broken := p
	broken.Journal.Entries = broken.Journal.Entries[:1]
	if _, err := e.apply(ctx, broken, nil); err == nil {
		t.Error("posting with an unbalanced journal accepted")
	}
}

func TestWallet_DepositWhoseExciseEatsItAllLeavesTheBalanceUnchangedButRecordsBothLines(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	u := e.newUser(t)
	p, err := ledger.Deposit(newRef(), u, 100, rule(t, "1"), true) // 100% excise: net movement 0
	bal, err := e.apply(ctx, p, err)
	if err != nil || bal != 0 {
		t.Fatalf("balance = %d, %v", bal, err)
	}
	var n int
	_ = e.pool.QueryRow(ctx, `SELECT count(*) FROM wallet.wallet_transactions WHERE user_id = $1`, u).Scan(&n)
	if n != 2 {
		t.Fatalf("%d wallet transactions, want 2 (deposit and excise)", n)
	}
}

func TestWallet_ReverseDepositRejectsNonPositiveAmounts(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	u := e.newUser(t)
	err := pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
		_, _, err := e.svc.ReverseDeposit(ctx, tx, newRef(), u, 0, 0)
		return err
	})
	if err == nil {
		t.Fatal("reversal of 0 accepted")
	}
}

// post applies a posting built by a ledger rule, passing the rule's error through.
func (e *env) post(p ledger.Posting, perr error) error {
	_, err := e.apply(context.Background(), p, perr)
	return err
}

func (e *env) debt(t testing.TB, user string) int64 {
	t.Helper()
	var d int64
	if err := pgx.BeginFunc(context.Background(), e.pool, func(tx pgx.Tx) error {
		var err error
		d, err = e.svc.Debt(context.Background(), tx, user)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return d
}

func (e *env) recover(t testing.TB, user, paymentID string) int64 {
	t.Helper()
	var n int64
	if err := pgx.BeginFunc(context.Background(), e.pool, func(tx pgx.Tx) error {
		var err error
		n, err = e.svc.RecoverDebt(context.Background(), tx, user, paymentID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestWallet_DebtFromAReversalIsRecoveredFromLaterDepositsOncePerDeposit(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	u, other := e.newUser(t), e.newUser(t)
	none := rule(t, "0")
	first := newRef()
	if err := e.post(ledger.Deposit(first, u, 400, none, true)); err != nil {
		t.Fatal(err)
	}
	if err := e.post(ledger.Stake(newRef(), u, 250)); err != nil { // 150 left
		t.Fatal(err)
	}
	if err := pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
		_, _, err := e.svc.ReverseDeposit(ctx, tx, first, u, 400, 0)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if d, b := e.debt(t, u), e.balance(t, u); d != 250 || b != 0 {
		t.Fatalf("after reversal: debt %d balance %d, want 250 and 0", d, b)
	}
	if d := e.debt(t, other); d != 0 {
		t.Fatalf("another player's debt = %d", d)
	}

	second := newRef() // a 100 deposit covers part of the debt
	if err := e.post(ledger.Deposit(second, u, 100, none, true)); err != nil {
		t.Fatal(err)
	}
	if n := e.recover(t, u, second); n != 100 {
		t.Fatalf("recovered %d, want 100", n)
	}
	if n := e.recover(t, u, second); n != 0 { // the same deposit never recovers twice
		t.Fatalf("second recovery from the same deposit = %d", n)
	}
	third := newRef() // a 500 deposit clears the rest and leaves 350
	if err := e.post(ledger.Deposit(third, u, 500, none, true)); err != nil {
		t.Fatal(err)
	}
	if n := e.recover(t, u, third); n != 150 {
		t.Fatalf("recovered %d, want 150", n)
	}
	if d, b := e.debt(t, u), e.balance(t, u); d != 0 || b != 350 {
		t.Fatalf("after recovery: debt %d balance %d, want 0 and 350", d, b)
	}
	if n := e.recover(t, u, newRef()); n != 0 {
		t.Fatalf("recovered %d without any debt", n)
	}
}

// A failing database call inside a money operation must surface as an error, never as "nothing to do".
func TestWallet_DatabaseFailuresInDebtOperationsAreReportedNotSwallowed(t *testing.T) {
	e := setup(t)
	u := e.newUser(t)
	ctx := context.Background()
	dead, cancel := context.WithCancel(ctx)
	cancel()
	for name, op := range map[string]func(pgx.Tx) error{
		"recover debt": func(tx pgx.Tx) error { _, err := e.svc.RecoverDebt(dead, tx, u, newRef()); return err },
		"debt":         func(tx pgx.Tx) error { _, err := e.svc.Debt(dead, tx, u); return err },
		"reverse":      func(tx pgx.Tx) error { _, _, err := e.svc.ReverseDeposit(dead, tx, newRef(), u, 100, 0); return err },
	} {
		tx, err := e.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := op(tx); err == nil {
			t.Errorf("%s: a failed database call was reported as success", name)
		}
		_ = tx.Rollback(ctx)
	}
}
