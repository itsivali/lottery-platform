package ledger_test

import (
	"testing"

	"pgregory.net/rapid"

	"github.com/itsivali/lottery-platform/internal/ledger"
)

func TestParseRate(t *testing.T) {
	cases := []struct {
		in      string
		wantE5  int64
		wantErr bool
	}{
		{"0", 0, false}, {"0.00000", 0, false}, {"0.15", 15000, false}, {"0.15000", 15000, false},
		{"1", 100000, false}, {"1.00000", 100000, false}, {"0.00001", 1, false},
		{"1.00001", 0, true}, {"-0.1", 0, true}, {"0.123456", 0, true}, {"abc", 0, true}, {"", 0, true}, {".5", 0, true},
	}
	for _, c := range cases {
		got, err := ledger.ParseRate(c.in)
		if (err != nil) != c.wantErr || (err == nil && got.E5() != c.wantE5) {
			t.Errorf("ParseRate(%q) = %v, %v; want e5=%d err=%v", c.in, got.E5(), err, c.wantE5, c.wantErr)
		}
	}
}

func TestApply_RoundingModes(t *testing.T) {
	cases := []struct {
		name   string
		amount int64
		rate   string
		mode   ledger.Rounding
		want   int64
	}{
		{"half up rounds .5 up", 5, "0.10000", ledger.HalfUp, 1},         // 0.5 -> 1
		{"half up rounds below .5 down", 4, "0.10000", ledger.HalfUp, 0}, // 0.4 -> 0
		{"half up exact", 100, "0.15000", ledger.HalfUp, 15},
		{"half up 1.5 -> 2", 15, "0.10000", ledger.HalfUp, 2},
		{"down truncates", 19, "0.10000", ledger.Down, 1},
		{"up ceils", 11, "0.10000", ledger.Up, 2},
		{"up exact stays", 10, "0.10000", ledger.Up, 1},
		{"zero amount", 0, "0.50000", ledger.HalfUp, 0},
		{"zero rate", 1_000_000, "0", ledger.HalfUp, 0},
		{"full rate", 777, "1", ledger.HalfUp, 777},
		{"large amount no overflow", 1_000_000_000_000, "0.16000", ledger.HalfUp, 160_000_000_000},
	}
	for _, c := range cases {
		r, err := ledger.ParseRate(c.rate)
		if err != nil {
			t.Fatal(err)
		}
		got, err := ledger.Apply(c.amount, r, c.mode)
		if err != nil || got != c.want {
			t.Errorf("%s: Apply(%d, %s, %s) = %d, %v; want %d", c.name, c.amount, c.rate, c.mode, got, err, c.want)
		}
	}
}

func TestApply_RejectsNegativeAndOversizedAmounts(t *testing.T) {
	r, _ := ledger.ParseRate("0.5")
	if _, err := ledger.Apply(-1, r, ledger.HalfUp); err == nil {
		t.Error("negative amount accepted")
	}
	if _, err := ledger.Apply(ledger.MaxAmountKES+1, r, ledger.HalfUp); err == nil {
		t.Error("oversized amount accepted")
	}
	if _, err := ledger.Apply(1, r, ledger.Rounding("banker")); err == nil {
		t.Error("unknown rounding accepted")
	}
}

func TestApply_Property_ResultIsFloorOrCeilOfExactProduct(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		amount := rapid.Int64Range(0, ledger.MaxAmountKES).Draw(t, "amount")
		e5 := rapid.Int64Range(0, 100000).Draw(t, "e5")
		mode := rapid.SampledFrom([]ledger.Rounding{ledger.HalfUp, ledger.Down, ledger.Up}).Draw(t, "mode")
		got, err := ledger.Apply(amount, ledger.RateFromE5(e5), mode)
		if err != nil {
			t.Fatal(err)
		}
		floor := amount * e5 / 100000
		rem := amount * e5 % 100000
		want := floor
		switch mode {
		case ledger.Up:
			if rem > 0 {
				want = floor + 1
			}
		case ledger.HalfUp:
			if rem*2 >= 100000 {
				want = floor + 1
			}
		}
		if got != want {
			t.Fatalf("Apply(%d, %d e5, %s) = %d, want %d", amount, e5, mode, got, want)
		}
		if got > amount {
			t.Fatalf("tax %d exceeds amount %d", got, amount)
		}
	})
}

func rate(t *testing.T, s string, m ledger.Rounding) ledger.TaxRule {
	t.Helper()
	r, err := ledger.ParseRate(s)
	if err != nil {
		t.Fatal(err)
	}
	return ledger.TaxRule{Code: "test", Rate: r, Rounding: m}
}

const (
	user = "00000000-0000-7000-8000-0000000000aa"
	ref  = "00000000-0000-7000-8000-0000000000bb"
)

func walletSum(p ledger.Posting) int64 {
	var s int64
	for _, w := range p.WalletTxs {
		s += w.AmountKES
	}
	return s
}

// playerLiability is credits minus debits on the player-wallet account for the player: must equal the wallet movement.
func playerLiability(p ledger.Posting) int64 {
	var s int64
	for _, e := range p.Journal.Entries {
		if e.Account == ledger.WalletLiability && e.UserID == user {
			if e.Dir == ledger.Credit {
				s += e.AmountKES
			} else {
				s -= e.AmountKES
			}
		}
	}
	return s
}

func TestPostingRules_EveryJournalBalancesAndMatchesTheWallet(t *testing.T) {
	excise := rate(t, "0.15", ledger.HalfUp)
	none := rate(t, "0", ledger.HalfUp)
	cases := []struct {
		name       string
		build      func() (ledger.Posting, error)
		wantWallet int64
	}{
		{"deposit, excise deducted", func() (ledger.Posting, error) { return ledger.Deposit(ref, user, 1000, excise, true) }, 850},
		{"deposit, excise absorbed", func() (ledger.Posting, error) { return ledger.Deposit(ref, user, 1000, excise, false) }, 1000},
		{"deposit, rounding remainder", func() (ledger.Posting, error) { return ledger.Deposit(ref, user, 33, excise, true) }, 28}, // 4.95 -> 5
		{"deposit with zero excise", func() (ledger.Posting, error) { return ledger.Deposit(ref, user, 100, none, true) }, 100},
		{"stake", func() (ledger.Posting, error) { return ledger.Stake(ref, user, 100) }, -100},
		{"stake refund", func() (ledger.Posting, error) { return ledger.StakeRefund(ref, user, 100) }, 100},
		{"withdrawal untaxed", func() (ledger.Posting, error) { return ledger.Withdrawal(ref, user, 500, none) }, -500},
		{"withdrawal taxed", func() (ledger.Posting, error) {
			return ledger.Withdrawal(ref, user, 500, rate(t, "0.2", ledger.HalfUp))
		}, -500},
		{"withdrawal failed", func() (ledger.Posting, error) {
			return ledger.WithdrawalFailed(ref, user, 500, rate(t, "0.2", ledger.HalfUp))
		}, 500},
		{"deposit reversal fully covered", func() (ledger.Posting, error) { return ledger.DepositReversal(ref, user, 400, 1000) }, -400},
		{"deposit reversal partly covered", func() (ledger.Posting, error) { return ledger.DepositReversal(ref, user, 400, 150) }, -150},
		{"deposit reversal with empty wallet", func() (ledger.Posting, error) { return ledger.DepositReversal(ref, user, 400, 0) }, 0},
	}
	for _, c := range cases {
		p, err := c.build()
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if err := p.Journal.Validate(); err != nil {
			t.Errorf("%s: invalid journal: %v", c.name, err)
		}
		if got := walletSum(p); got != c.wantWallet {
			t.Errorf("%s: wallet movement = %d, want %d", c.name, got, c.wantWallet)
		}
		if got := playerLiability(p); got != c.wantWallet {
			t.Errorf("%s: ledger player liability movement = %d, want %d (must equal wallet)", c.name, got, c.wantWallet)
		}
	}
}

func TestDepositReversal_ShortfallBecomesPlayerDebt(t *testing.T) {
	p, err := ledger.DepositReversal(ref, user, 400, 150)
	if err != nil {
		t.Fatal(err)
	}
	var debt int64
	for _, e := range p.Journal.Entries {
		if e.Account == ledger.PlayerDebt && e.Dir == ledger.Debit {
			debt += e.AmountKES
		}
	}
	if debt != 250 {
		t.Fatalf("player debt = %d, want 250", debt)
	}
}

func TestPrizePayout_DoesNotTouchTheWallet(t *testing.T) {
	p, err := ledger.PrizePayout(ref, "weekly", 10_000, rate(t, "0.2", ledger.HalfUp))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.WalletTxs) != 0 {
		t.Fatalf("prize payout produced wallet transactions: %v", p.WalletTxs)
	}
	if err := p.Journal.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.PrizePayout(ref, "yearly", 1, rate(t, "0", ledger.HalfUp)); err == nil {
		t.Fatal("unknown draw type accepted")
	}
}

func TestPostingRules_RejectNonPositiveAmounts(t *testing.T) {
	none := rate(t, "0", ledger.HalfUp)
	for _, amt := range []int64{0, -5} {
		if _, err := ledger.Deposit(ref, user, amt, none, true); err == nil {
			t.Errorf("Deposit(%d) accepted", amt)
		}
		if _, err := ledger.Stake(ref, user, amt); err == nil {
			t.Errorf("Stake(%d) accepted", amt)
		}
		if _, err := ledger.Withdrawal(ref, user, amt, none); err == nil {
			t.Errorf("Withdrawal(%d) accepted", amt)
		}
	}
}

func TestJournalValidate_RejectsUnbalancedAndMalformed(t *testing.T) {
	good := ledger.Journal{Kind: "k", RefType: "t", RefID: ref, Entries: []ledger.Entry{
		{Account: ledger.MpesaClearing, Dir: ledger.Debit, AmountKES: 5},
		{Account: ledger.WalletLiability, Dir: ledger.Credit, AmountKES: 5, UserID: user},
	}}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := map[string]func(j *ledger.Journal){
		"unbalanced":      func(j *ledger.Journal) { j.Entries[1].AmountKES = 4 },
		"single entry":    func(j *ledger.Journal) { j.Entries = j.Entries[:1] },
		"zero amount":     func(j *ledger.Journal) { j.Entries[0].AmountKES, j.Entries[1].AmountKES = 0, 0 },
		"bad direction":   func(j *ledger.Journal) { j.Entries[0].Dir = "X" },
		"unknown account": func(j *ledger.Journal) { j.Entries[0].Account = "NOPE" },
		"missing ref":     func(j *ledger.Journal) { j.RefID = "" },
	}
	for name, mutate := range bad {
		j := good
		j.Entries = append([]ledger.Entry(nil), good.Entries...)
		mutate(&j)
		if err := j.Validate(); err == nil {
			t.Errorf("%s: Validate accepted it", name)
		}
	}
}

func TestPostingRules_Property_AnyAmountAndRuleBalances(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		amt := rapid.Int64Range(1, ledger.MaxAmountKES).Draw(t, "amt")
		bal := rapid.Int64Range(0, ledger.MaxAmountKES).Draw(t, "balance")
		rl := ledger.TaxRule{
			Code:     "x",
			Rate:     ledger.RateFromE5(rapid.Int64Range(0, 100000).Draw(t, "e5")),
			Rounding: rapid.SampledFrom([]ledger.Rounding{ledger.HalfUp, ledger.Down, ledger.Up}).Draw(t, "mode"),
		}
		builds := []func() (ledger.Posting, error){
			func() (ledger.Posting, error) { return ledger.Deposit(ref, user, amt, rl, true) },
			func() (ledger.Posting, error) { return ledger.Deposit(ref, user, amt, rl, false) },
			func() (ledger.Posting, error) { return ledger.Withdrawal(ref, user, amt, rl) },
			func() (ledger.Posting, error) { return ledger.WithdrawalFailed(ref, user, amt, rl) },
			func() (ledger.Posting, error) { return ledger.DepositReversal(ref, user, amt, bal) },
			func() (ledger.Posting, error) { return ledger.PrizePayout(ref, "daily", amt, rl) },
		}
		for i, b := range builds {
			p, err := b()
			if err != nil {
				t.Fatalf("builder %d: %v", i, err)
			}
			if err := p.Journal.Validate(); err != nil {
				t.Fatalf("builder %d: %v", i, err)
			}
			if walletSum(p) != playerLiability(p) {
				t.Fatalf("builder %d: wallet %d != player liability %d", i, walletSum(p), playerLiability(p))
			}
		}
	})
}

func TestPosting_WalletDeltaSumsItsTransactions(t *testing.T) {
	p, err := ledger.Deposit(ref, user, 1000, rate(t, "0.15", ledger.HalfUp), true)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.WalletDelta(); got != 850 {
		t.Fatalf("WalletDelta = %d, want 850", got)
	}
}

func TestPostingRules_RejectOutOfRangeAmountsAndUnknownRounding(t *testing.T) {
	none := rate(t, "0", ledger.HalfUp)
	bad := ledger.TaxRule{Rate: none.Rate, Rounding: "banker"}
	if _, err := ledger.StakeRefund(ref, user, 0); err == nil {
		t.Error("StakeRefund(0) accepted")
	}
	if _, err := ledger.WithdrawalFailed(ref, user, 0, none); err == nil {
		t.Error("WithdrawalFailed(0) accepted")
	}
	if _, err := ledger.DepositReversal(ref, user, 0, 10); err == nil {
		t.Error("DepositReversal(0) accepted")
	}
	if _, err := ledger.DepositReversal(ref, user, 10, -1); err == nil {
		t.Error("DepositReversal with negative balance accepted")
	}
	if _, err := ledger.PrizePayout(ref, "daily", 0, none); err == nil {
		t.Error("PrizePayout(0) accepted")
	}
	if _, err := ledger.Deposit(ref, user, 10, bad, true); err == nil {
		t.Error("Deposit with unknown rounding accepted")
	}
	if _, err := ledger.Withdrawal(ref, user, 10, bad); err == nil {
		t.Error("Withdrawal with unknown rounding accepted")
	}
	if _, err := ledger.WithdrawalFailed(ref, user, 10, bad); err == nil {
		t.Error("WithdrawalFailed with unknown rounding accepted")
	}
	if _, err := ledger.PrizePayout(ref, "daily", 10, bad); err == nil {
		t.Error("PrizePayout with unknown rounding accepted")
	}
	if _, err := ledger.Stake(ref, user, ledger.MaxAmountKES+1); err == nil {
		t.Error("oversized stake accepted")
	}
}
