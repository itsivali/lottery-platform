//go:build integration

package game_test

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/itsivali/lottery-platform/internal/audit"
	"github.com/itsivali/lottery-platform/internal/game"
	"github.com/itsivali/lottery-platform/internal/game/fair"
	"github.com/itsivali/lottery-platform/internal/identity"
	"github.com/itsivali/lottery-platform/internal/ledger"
	"github.com/itsivali/lottery-platform/internal/platform/clock"
	"github.com/itsivali/lottery-platform/internal/platform/crypto"
	"github.com/itsivali/lottery-platform/internal/platform/testpg"
	"github.com/itsivali/lottery-platform/internal/rg"
	"github.com/itsivali/lottery-platform/internal/wallet"
)

// 2026-10-24 23:30 Nairobi (20:30 UTC): late enough to cross midnight in a test.
var start = time.Date(2026, 10, 24, 20, 30, 0, 0, time.UTC)

type env struct {
	pool   *pgxpool.Pool
	clk    *clock.Manual
	svc    *game.Service
	rg     *rg.Service
	wallet *wallet.Service
	ls     *ledger.Store
}

func setup(t *testing.T) *env {
	t.Helper()
	pool := testpg.Pool(t, "app_api")
	clk := clock.NewManual(start)
	aw := audit.NewWriter()
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	kr, err := crypto.NewKeyring(1, map[byte][]byte{1: k})
	if err != nil {
		t.Fatal(err)
	}
	ids := identity.NewService(pool, clk, nil, aw, identity.Config{AuthKey: []byte("0123456789abcdef0123456789abcdef")})
	r := rg.NewService(pool, clk, aw, rg.Config{CoolingOff: 24 * time.Hour})
	ls := ledger.NewStore()
	w := wallet.NewService(ls)
	return &env{pool: pool, clk: clk, rg: r, wallet: w, ls: ls,
		svc: game.NewService(pool, clk, ids, r, w, kr, wallet.ErrInsufficientFunds)}
}

var seq atomic.Int64

// player is a KYC-verified account with a proved phone and a wallet holding credit (deposit net of 5% excise).
func (e *env) player(t *testing.T, depositKES int64) string {
	t.Helper()
	n := seq.Add(1)
	var id string
	if err := e.pool.QueryRow(context.Background(),
		`INSERT INTO identity.users (msisdn, kyc_status, national_id_hash, password_hash, msisdn_verified_at)
		 VALUES ($1, 'verified', $2, 'x', now()) RETURNING id::text`,
		fmt.Sprintf("2547%08d", n), []byte(fmt.Sprint("game-id-", n))).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if depositKES > 0 {
		e.deposit(t, id, depositKES)
	}
	return id
}

// deposit credits gross net of the 5% excise and returns the payment id the deposit is keyed on.
func (e *env) deposit(t *testing.T, user string, gross int64) string {
	t.Helper()
	var paymentID string
	err := pgx.BeginFunc(context.Background(), e.pool, func(tx pgx.Tx) error {
		rule, err := e.ls.ActiveRule(context.Background(), tx, "deposit_excise", start)
		if err != nil {
			return err
		}
		if err := tx.QueryRow(context.Background(), `SELECT platform.uuid_v7()::text`).Scan(&paymentID); err != nil {
			return err
		}
		p, err := ledger.Deposit(paymentID, user, gross, rule, true)
		if err != nil {
			return err
		}
		_, err = e.wallet.Apply(context.Background(), tx, p)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return paymentID
}

func (e *env) balance(t *testing.T, user string) int64 {
	t.Helper()
	var b int64
	if err := e.pool.QueryRow(context.Background(), `SELECT balance_kes FROM wallet.wallets WHERE user_id = $1`, user).Scan(&b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPlay_DebitsThePriceIssuesATicketAndSpinsFairly(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	u := e.player(t, 1000) // credited 950
	f, err := e.svc.Fairness(ctx, u)
	if err != nil || len(f.Active.Commitment) != 64 || f.Active.NextNonce != 0 {
		t.Fatalf("fairness before the first play: %+v %v", f, err)
	}

	res, err := e.svc.Play(ctx, u, "k1")
	if err != nil {
		t.Fatal(err)
	}
	p := res.Play
	if p.PriceKES != 100 || p.Seq != 1 || res.BalanceKES != 850 || e.balance(t, u) != 850 || res.Replayed {
		t.Fatalf("play = %+v balance %d", p, res.BalanceKES)
	}
	if p.PlayDate.Format(time.DateOnly) != "2026-10-24" { // 23:30 in Nairobi
		t.Fatalf("play date %s", p.PlayDate)
	}
	if p.Spin.Commitment != f.Active.Commitment || p.Spin.Nonce != 0 || p.Spin.Slots != 12 || p.Spin.LuckySlots != 1 ||
		p.Spin.Lucky != (p.Spin.Slot < 1) {
		t.Fatalf("spin = %+v", p.Spin)
	}
	if res.Today.Plays != 1 || res.Today.PlaysLeft != 3 || res.Today.PriceKES != 100 || len(res.Today.Tickets) != 1 {
		t.Fatalf("today = %+v", res.Today)
	}
	wantChances := 1
	if p.Spin.Lucky {
		wantChances = 2
	}
	if res.Today.Chances != wantChances {
		t.Fatalf("chances %d, want %d", res.Today.Chances, wantChances)
	}

	// the stake is in the ledger: STAKES_REVENUE holds the price
	var revenue int64
	if err := e.pool.QueryRow(ctx, `SELECT coalesce(sum(CASE e.direction WHEN 'C' THEN e.amount_kes ELSE -e.amount_kes END),0)
		FROM ledger.ledger_entries e JOIN ledger.ledger_journals j ON j.id = e.journal_id JOIN ledger.ledger_accounts a ON a.id = e.account_id
		WHERE a.code = 'STAKES_REVENUE' AND j.reference_id = $1::uuid`, p.TicketID).Scan(&revenue); err != nil || revenue != 100 {
		t.Fatalf("stakes revenue %d %v", revenue, err)
	}

	// rotating reveals the seed: the spin can be checked against the commitment shown before it
	r, err := e.svc.Rotate(ctx, u, "my-own-seed")
	if err != nil || len(r.Rotated) != 1 || r.Active.ClientSeed != "my-own-seed" || r.Active.Commitment == f.Active.Commitment {
		t.Fatalf("rotate: %+v %v", r, err)
	}
	old := r.Rotated[0]
	slot, err := fair.Verify(old.RevealedSeed, f.Active.Commitment, old.ClientSeed, p.Spin.Nonce, p.Spin.Slots)
	if err != nil || slot != p.Spin.Slot || old.NextNonce != 1 {
		t.Fatalf("verify: slot %d want %d, %v (%+v)", slot, p.Spin.Slot, err, old)
	}

	// the next play uses the new chain from nonce 0
	res2, err := e.svc.Play(ctx, u, "k2")
	if err != nil || res2.Play.Spin.ClientSeed != "my-own-seed" || res2.Play.Spin.Nonce != 0 || res2.Play.Spin.Commitment != r.Active.Commitment {
		t.Fatalf("second play: %+v %v", res2.Play.Spin, err)
	}
}

func TestPlay_SameKeyChargesOnce(t *testing.T) {
	e := setup(t)
	u := e.player(t, 1000)
	a, err := e.svc.Play(context.Background(), u, "same")
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.svc.Play(context.Background(), u, "same")
	if err != nil || !b.Replayed || b.Play.TicketID != a.Play.TicketID || b.BalanceKES != 850 || e.balance(t, u) != 850 || b.Today.Plays != 1 {
		t.Fatalf("replay: %+v %v", b, err)
	}
	if _, err := e.svc.Play(context.Background(), u, ""); !errors.Is(err, game.ErrIdempotencyKey) {
		t.Fatalf("no key: %v", err)
	}
}

func TestPlay_FourPerNairobiDayEvenUnderConcurrency(t *testing.T) {
	e := setup(t)
	u := e.player(t, 2000)
	var ok, capped atomic.Int32
	var wg sync.WaitGroup
	for i := range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := e.svc.Play(context.Background(), u, fmt.Sprint("race-", i))
			switch {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, game.ErrDailyCap):
				capped.Add(1)
			default:
				t.Errorf("play: %v", err)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 4 || capped.Load() != 6 || e.balance(t, u) != 1900-400 {
		t.Fatalf("ok %d capped %d balance %d", ok.Load(), capped.Load(), e.balance(t, u))
	}
	today, _ := e.svc.Today(context.Background(), u)
	seqs := map[int]bool{}
	for _, p := range today.Tickets {
		seqs[p.Seq] = true
	}
	if today.Plays != 4 || today.PlaysLeft != 0 || len(seqs) != 4 {
		t.Fatalf("today = %+v", today)
	}
	// nonces are consecutive on the chain
	var nonces []int64
	for _, p := range today.Tickets {
		nonces = append(nonces, p.Spin.Nonce)
	}
	f, _ := e.svc.Fairness(context.Background(), u)
	if f.Active.NextNonce != 4 {
		t.Fatalf("next nonce %d, nonces %v", f.Active.NextNonce, nonces)
	}

	// after midnight in Nairobi the cap resets
	e.clk.Advance(31 * time.Minute)
	res, err := e.svc.Play(context.Background(), u, "tomorrow")
	if err != nil || res.Play.PlayDate.Format(time.DateOnly) != "2026-10-25" || res.Today.Plays != 1 {
		t.Fatalf("next day: %+v %v", res, err)
	}
}

func TestPlay_ChancesFollowTheTiersAndDoubleOnALuckyDay(t *testing.T) {
	e := setup(t)
	u := e.player(t, 1000)
	want := map[int]int{1: 1, 2: 2, 3: 3, 4: 5}
	lucky := false
	for i := 1; i <= 4; i++ {
		res, err := e.svc.Play(context.Background(), u, fmt.Sprint("t", i))
		if err != nil {
			t.Fatal(err)
		}
		lucky = lucky || res.Play.Spin.Lucky
		w := want[i]
		if lucky {
			w *= 2
		}
		if res.Today.Chances != w || res.Today.Lucky != lucky {
			t.Fatalf("after %d plays: chances %d lucky %v, want %d %v", i, res.Today.Chances, res.Today.Lucky, w, lucky)
		}
	}
	// force the lucky case: a day row marked lucky doubles the tier
	if _, err := e.pool.Exec(context.Background(), `UPDATE tickets.player_days SET lucky = true WHERE user_id = $1`, u); err != nil {
		t.Fatal(err)
	}
	if today, _ := e.svc.Today(context.Background(), u); today.Chances != 10 {
		t.Fatalf("lucky day of 4 plays: %d chances", today.Chances)
	}
}

func TestPlay_RefusesWhoMayNotPlayAndChargesNothing(t *testing.T) {
	e := setup(t)
	ctx := context.Background()

	broke := e.player(t, 100) // credited 95, below the price
	if _, err := e.svc.Play(ctx, broke, "a"); !errors.Is(err, game.ErrInsufficientFunds) {
		t.Fatalf("low balance: %v", err)
	}
	if e.balance(t, broke) != 95 {
		t.Fatal("charged on a refused play")
	}

	unproved := e.player(t, 1000)
	if _, err := e.pool.Exec(ctx, `UPDATE identity.users SET msisdn_verified_at = NULL WHERE id = $1`, unproved); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Play(ctx, unproved, "a"); !errors.Is(err, identity.ErrPhoneNotVerified) {
		t.Fatalf("unproved phone: %v", err)
	}

	excluded := e.player(t, 1000)
	if err := e.rg.Timeout(ctx, excluded, "24h"); err != nil {
		t.Fatal(err)
	}
	var re *rg.RestrictedError
	if _, err := e.svc.Play(ctx, excluded, "a"); !errors.As(err, &re) {
		t.Fatalf("timed out: %v", err)
	}

	// a debtor: spends most of the credit, then the deposit is reversed, and what is missing becomes debt
	debtor := e.player(t, 0)
	pid := e.deposit(t, debtor, 1000)
	err := pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
		if _, err := e.wallet.Apply(ctx, tx, mustStake(t, debtor, 900)); err != nil {
			return err
		}
		_, _, err := e.wallet.ReverseDeposit(ctx, tx, pid, debtor, 1000, 50)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	e.deposit(t, debtor, 2000) // credit outside the payments module, so the debt is still owed
	if debt, err := e.debt(t, debtor); err != nil || debt != 900 {
		t.Fatalf("debt %d %v", debt, err)
	}
	if _, err := e.svc.Play(ctx, debtor, "a"); !errors.Is(err, game.ErrInDebt) {
		t.Fatalf("in debt: %v", err)
	}
	for _, u := range []string{broke, unproved, excluded, debtor} {
		if today, _ := e.svc.Today(ctx, u); today.Plays != 0 {
			t.Fatalf("a refused play was counted for %s", u)
		}
	}
}

func (e *env) debt(t *testing.T, user string) (int64, error) {
	t.Helper()
	var d int64
	err := pgx.BeginFunc(context.Background(), e.pool, func(tx pgx.Tx) error {
		var err error
		d, err = e.wallet.Debt(context.Background(), tx, user)
		return err
	})
	return d, err
}

func mustStake(t *testing.T, user string, amount int64) ledger.Posting {
	t.Helper()
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	id := fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
	p, err := ledger.Stake(id, user, amount)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRotate_RefusesABadClientSeedAndWorksWithoutAChain(t *testing.T) {
	e := setup(t)
	u := e.player(t, 0)
	if _, err := e.svc.Rotate(context.Background(), u, "bad seed!"); !errors.Is(err, fair.ErrBadClientSeed) {
		t.Fatalf("bad seed: %v", err)
	}
	f, err := e.svc.Rotate(context.Background(), u, "")
	if err != nil || len(f.Rotated) != 0 || f.Active.Commitment == "" {
		t.Fatalf("first rotate: %+v %v", f, err)
	}
}

func TestFairness_ConcurrentFirstRequestsCreateOneChain(t *testing.T) {
	e := setup(t)
	u := e.player(t, 0)
	var wg sync.WaitGroup
	commits := make([]string, 8)
	for i := range commits {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f, err := e.svc.Fairness(context.Background(), u)
			if err != nil {
				t.Error(err)
				return
			}
			commits[i] = f.Active.Commitment
		}()
	}
	wg.Wait()
	for _, c := range commits {
		if c != commits[0] {
			t.Fatalf("different active chains: %v", commits)
		}
	}
}

func TestPlay_ConcurrentRequestsWithOneKeyMakeOnePlay(t *testing.T) {
	e := setup(t)
	u := e.player(t, 1000)
	var wg sync.WaitGroup
	ids := make([]string, 6)
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := e.svc.Play(context.Background(), u, "one-key")
			if err != nil {
				t.Error(err)
				return
			}
			ids[i] = res.Play.TicketID
		}()
	}
	wg.Wait()
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("different tickets for one key: %v", ids)
		}
	}
	if e.balance(t, u) != 850 {
		t.Fatalf("charged more than once: balance %d", e.balance(t, u))
	}
}

func TestPlay_WithoutAnOddsVersionInForceNothingIsSold(t *testing.T) {
	e := setup(t)
	u := e.player(t, 1000)
	e.clk.Set(time.Date(1999, 12, 31, 12, 0, 0, 0, time.UTC)) // before the first odds version
	if _, err := e.svc.Play(context.Background(), u, "k"); !errors.Is(err, game.ErrNoOdds) {
		t.Fatalf("play: %v", err)
	}
	if _, err := e.svc.Today(context.Background(), u); !errors.Is(err, game.ErrNoOdds) {
		t.Fatalf("today: %v", err)
	}
}

func TestSeedChain_TamperingIsRefused(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	u := e.player(t, 1000)
	if _, err := e.svc.Fairness(ctx, u); err != nil {
		t.Fatal(err)
	}
	// a commitment changed after the fact: the seed no longer matches what was shown, so it is not revealed as if it did
	if _, err := e.pool.Exec(ctx, `UPDATE game.seed_chains SET commitment = repeat('0', 64) WHERE user_id = $1`, u); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Rotate(ctx, u, ""); err == nil {
		t.Fatal("rotated a chain whose seed does not match its commitment")
	}
	// an encrypted seed that does not decrypt (wrong key or edited): no spin and no rotation
	if _, err := e.pool.Exec(ctx, `UPDATE game.seed_chains SET seed_enc = '\x0001' WHERE user_id = $1`, u); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Play(ctx, u, "k"); err == nil {
		t.Fatal("played with a seed that does not decrypt")
	}
	if _, err := e.svc.Rotate(ctx, u, ""); err == nil {
		t.Fatal("rotated a seed that does not decrypt")
	}
	if e.balance(t, u) != 950 {
		t.Fatal("charged for a failed play")
	}
}

func TestService_DatabaseFailuresSurfaceAsErrors(t *testing.T) {
	e := setup(t)
	u := e.player(t, 1000)
	if _, err := e.svc.Play(context.Background(), u, "k"); err != nil { // a chain and a day exist
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.svc.Play(ctx, u, "k2"); err == nil {
		t.Error("play on a cancelled context")
	}
	if _, err := e.svc.Today(ctx, u); err == nil {
		t.Error("today on a cancelled context")
	}
	if _, err := e.svc.Fairness(ctx, u); err == nil {
		t.Error("fairness on a cancelled context")
	}
	if _, err := e.svc.Rotate(ctx, u, ""); err == nil {
		t.Error("rotate on a cancelled context")
	}
}

func TestPlay_AReusedNonceIsRefusedAndNothingIsCharged(t *testing.T) {
	e := setup(t)
	u := e.player(t, 1000)
	if _, err := e.svc.Play(context.Background(), u, "a"); err != nil {
		t.Fatal(err)
	}
	// wind the chain back: the next spin would reuse nonce 0, which the database refuses (one spin per chain and nonce)
	if _, err := e.pool.Exec(context.Background(), `UPDATE game.seed_chains SET next_nonce = 0 WHERE user_id = $1`, u); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Play(context.Background(), u, "b"); err == nil {
		t.Fatal("a nonce was used twice")
	}
	if e.balance(t, u) != 850 {
		t.Fatalf("balance %d after the refused play", e.balance(t, u))
	}
}

type brokenClock struct{}

func (brokenClock) Now(context.Context, clock.Querier) (time.Time, error) {
	return time.Time{}, errors.New("clock unavailable")
}

// brokenWallet fails the call named in fail and delegates the rest.
type brokenWallet struct {
	*wallet.Service
	fail string
}

var errWallet = errors.New("wallet unavailable")

func (w brokenWallet) Debt(ctx context.Context, tx pgx.Tx, u string) (int64, error) {
	if w.fail == "debt" {
		return 0, errWallet
	}
	return w.Service.Debt(ctx, tx, u)
}

func (w brokenWallet) Apply(ctx context.Context, tx pgx.Tx, p ledger.Posting) (int64, error) {
	if w.fail == "apply" {
		return 0, errWallet
	}
	return w.Service.Apply(ctx, tx, p)
}

func (w brokenWallet) Balance(ctx context.Context, tx pgx.Tx, u string) (int64, error) {
	if w.fail == "balance" {
		return 0, errWallet
	}
	return w.Service.Balance(ctx, tx, u)
}

func TestService_DependencyFailuresAbortThePlay(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	u := e.player(t, 1000)
	kr, _ := crypto.NewKeyring(1, map[byte][]byte{1: make([]byte, 32)})
	ids := identity.NewService(e.pool, e.clk, nil, audit.NewWriter(), identity.Config{AuthKey: []byte("0123456789abcdef0123456789abcdef")})

	noClock := game.NewService(e.pool, brokenClock{}, ids, e.rg, e.wallet, kr, wallet.ErrInsufficientFunds)
	if _, err := noClock.Play(ctx, u, "c"); err == nil {
		t.Error("play without a clock")
	}
	if _, err := noClock.Today(ctx, u); err == nil {
		t.Error("today without a clock")
	}
	if _, err := noClock.Fairness(ctx, u); err == nil {
		t.Error("fairness without a clock")
	}
	if _, err := noClock.Rotate(ctx, u, ""); err == nil {
		t.Error("rotate without a clock")
	}
	for _, fail := range []string{"debt", "apply"} {
		svc := game.NewService(e.pool, e.clk, ids, e.rg, brokenWallet{e.wallet, fail}, kr, wallet.ErrInsufficientFunds)
		if _, err := svc.Play(ctx, u, "w-"+fail); !errors.Is(err, errWallet) {
			t.Errorf("%s failing: %v", fail, err)
		}
	}
	if e.balance(t, u) != 950 {
		t.Fatal("a failed play charged the player")
	}
	ok := game.NewService(e.pool, e.clk, ids, e.rg, e.wallet, kr, wallet.ErrInsufficientFunds)
	if _, err := ok.Play(ctx, u, "w-replay"); err != nil {
		t.Fatal(err)
	}
	broken := game.NewService(e.pool, e.clk, ids, e.rg, brokenWallet{e.wallet, "balance"}, kr, wallet.ErrInsufficientFunds)
	if _, err := broken.Play(ctx, u, "w-replay"); !errors.Is(err, errWallet) {
		t.Errorf("replay with the balance failing: %v", err)
	}
}
