//go:build integration

package ledger_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/itsivali/lottery-platform/internal/ledger"
	"github.com/itsivali/lottery-platform/internal/platform/testpg"
)

func TestStore_ActiveRulePicksTheLatestVersionInForce(t *testing.T) {
	pool := testpg.Pool(t, "app_api")
	ctx := context.Background()
	s := ledger.NewStore()
	mustInsert := func(rate, from string) {
		// tax_rules are written by the back-office/migrator, never updated: insert as the owner via a fresh superuser conn
		t.Helper()
		_, err := pool.Exec(ctx, `INSERT INTO ledger.tax_rules (code, rate, point_of_taxation, effective_from, created_by) VALUES ('excise',$1,'deposit',$2,'test')`, rate, from)
		if err != nil {
			t.Fatal(err)
		}
	}
	read := func(on string) (ledger.TaxRule, error) {
		d, _ := time.Parse(time.DateOnly, on)
		var r ledger.TaxRule
		err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			var err error
			r, err = s.ActiveRule(ctx, tx, "excise", d)
			return err
		})
		return r, err
	}
	if _, err := read("2026-10-01"); !errors.Is(err, ledger.ErrNoTaxRule) {
		t.Fatalf("no rule configured: err = %v, want ErrNoTaxRule", err)
	}
	mustInsert("0.10000", "2026-01-01")
	mustInsert("0.15000", "2026-07-01")
	for on, want := range map[string]int64{"2026-03-01": 10000, "2026-07-01": 15000, "2026-10-21": 15000} {
		r, err := read(on)
		if err != nil || r.Rate.E5() != want {
			t.Errorf("on %s: rate e5 = %d, %v; want %d", on, r.Rate.E5(), err, want)
		}
	}
	if _, err := read("2025-12-31"); !errors.Is(err, ledger.ErrNoTaxRule) {
		t.Errorf("before first version: err = %v, want ErrNoTaxRule", err)
	}
}

func TestStore_PostRefusesAnUnbalancedJournalBeforeTouchingTheDatabase(t *testing.T) {
	pool := testpg.Pool(t, "app_api")
	ctx := context.Background()
	bad := ledger.Journal{Kind: "k", RefType: "t", RefID: "00000000-0000-7000-8000-000000000001", Entries: []ledger.Entry{
		{Account: ledger.MpesaClearing, Dir: ledger.Debit, AmountKES: 10},
		{Account: ledger.WalletLiability, Dir: ledger.Credit, AmountKES: 9},
	}}
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		_, err := ledger.NewStore().Post(ctx, tx, bad)
		return err
	})
	if err == nil {
		t.Fatal("unbalanced journal posted")
	}
}
