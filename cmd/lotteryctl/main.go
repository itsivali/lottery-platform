// Command lotteryctl is the admin CLI: migrations and the payments operator actions (reverse a top-up M-Pesa took
// back, resolve an unconfirmed top-up from the statement, run the internal reconciliation). It needs DATABASE_URL;
// operator actions are audited as staff "operator:<name>".
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/itsivali/lottery-platform/internal/audit"
	"github.com/itsivali/lottery-platform/internal/identity"
	"github.com/itsivali/lottery-platform/internal/ledger"
	"github.com/itsivali/lottery-platform/internal/payments"
	"github.com/itsivali/lottery-platform/internal/platform/clock"
	"github.com/itsivali/lottery-platform/internal/platform/config"
	"github.com/itsivali/lottery-platform/internal/platform/db"
	"github.com/itsivali/lottery-platform/internal/rg"
	"github.com/itsivali/lottery-platform/internal/wallet"
)

const usage = `usage:
  lotteryctl migrate up|down
  lotteryctl payments reconcile
  lotteryctl payments reverse -receipt R -reason TEXT -operator NAME
  lotteryctl payments resolve -payment ID -receipt R -amount KES -operator NAME`

type command struct {
	name                               string
	receipt, reason, operator, payment string
	amount                             int64
}

func parse(args []string) (command, error) {
	bad := errors.New(usage)
	if len(args) < 2 {
		return command{}, bad
	}
	switch args[0] + " " + args[1] {
	case "migrate up", "migrate down", "payments reconcile":
		if len(args) != 2 {
			return command{}, bad
		}
		return command{name: args[0] + " " + args[1]}, nil
	case "payments reverse", "payments resolve":
		c := command{name: args[0] + " " + args[1]}
		fs := flag.NewFlagSet(c.name, flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		fs.StringVar(&c.receipt, "receipt", "", "M-Pesa receipt")
		fs.StringVar(&c.operator, "operator", "", "who is acting (audited)")
		fs.StringVar(&c.reason, "reason", "", "why the top-up was reversed")
		fs.StringVar(&c.payment, "payment", "", "payment id")
		fs.Int64Var(&c.amount, "amount", 0, "amount on the statement, whole KES")
		if err := fs.Parse(args[2:]); err != nil || fs.NArg() != 0 || c.receipt == "" || c.operator == "" {
			return command{}, bad
		}
		if c.name == "payments reverse" && (c.reason == "" || c.payment != "" || c.amount != 0) {
			return command{}, bad
		}
		if c.name == "payments resolve" && (c.payment == "" || c.amount <= 0 || c.reason != "") {
			return command{}, bad
		}
		c.operator = "operator:" + c.operator
		return c, nil
	}
	return command{}, bad
}

func main() {
	cmd, err := parse(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := run(context.Background(), cmd, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cmd command, out io.Writer) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	switch cmd.name {
	case "migrate up":
		return db.MigrateUp(ctx, cfg.DatabaseURL)
	case "migrate down":
		return db.MigrateDown(ctx, cfg.DatabaseURL)
	}

	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	// Operator actions need no Daraja, job queue or secrets: they work on recorded payments, the ledger and wallets.
	aw := audit.NewWriter()
	clk := clock.Postgres{}
	ls := ledger.NewStore()
	svc := payments.NewService(pool, clk, nil, nil, identity.NewService(pool, clk, nil, aw, identity.Config{}),
		rg.NewService(pool, clk, aw, rg.Config{CoolingOff: 24 * time.Hour}), wallet.NewService(ls), ls, aw, payments.Config{})

	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	switch cmd.name {
	case "payments reconcile":
		r, err := svc.Reconcile(ctx)
		if err != nil {
			return err
		}
		if err := enc.Encode(r); err != nil {
			return err
		}
		if !r.OK {
			return errors.New("reconciliation found mismatches")
		}
		return nil
	case "payments reverse":
		r, err := svc.Reverse(ctx, cmd.receipt, cmd.reason, cmd.operator)
		if err != nil {
			return err
		}
		return enc.Encode(r)
	case "payments resolve":
		if err := svc.ResolveUnconfirmed(ctx, cmd.payment, cmd.receipt, cmd.amount, cmd.operator); err != nil {
			return err
		}
		_, err := fmt.Fprintln(out, "credited", cmd.payment, "from receipt", cmd.receipt)
		return err
	}
	return fmt.Errorf("unknown command %q", cmd.name)
}
