// Command lotteryctl is the admin CLI: migrations and the payments operator actions (reverse a top-up M-Pesa took
// back, resolve an unconfirmed top-up from the statement, credit or refund a paybill payment held in suspense, check an
// unverified paybill payment again, run the internal reconciliation). It needs DATABASE_URL; operator actions are
// audited as staff "operator:<name>". Registering the paybill URLs and simulating a sandbox payment need the api's
// Daraja configuration.
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

	"github.com/itsivali/lottery-platform/internal/app"
	"github.com/itsivali/lottery-platform/internal/audit"
	"github.com/itsivali/lottery-platform/internal/identity"
	"github.com/itsivali/lottery-platform/internal/ledger"
	"github.com/itsivali/lottery-platform/internal/payments"
	"github.com/itsivali/lottery-platform/internal/payments/daraja"
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
  lotteryctl payments resolve -payment ID -receipt R -amount KES -operator NAME
  lotteryctl payments suspense-credit -receipt R -user ID -operator NAME
  lotteryctl payments suspense-refund -receipt R -refund-ref REF -operator NAME
  lotteryctl payments c2b-recheck -receipt R -operator NAME
  lotteryctl payments c2b-register
  lotteryctl payments c2b-simulate -amount KES -msisdn 2547XXXXXXXX -account ACCOUNT`

type command struct {
	name                               string
	receipt, reason, operator, payment string
	user, refundRef, msisdn, account   string
	amount                             int64
}

// flagsOf lists, per command, the flags it requires; every other flag must be absent.
var flagsOf = map[string][]string{
	"migrate up": nil, "migrate down": nil, "payments reconcile": nil, "payments c2b-register": nil,
	"payments reverse":         {"receipt", "reason", "operator"},
	"payments resolve":         {"payment", "receipt", "amount", "operator"},
	"payments suspense-credit": {"receipt", "user", "operator"},
	"payments suspense-refund": {"receipt", "refund-ref", "operator"},
	"payments c2b-recheck":     {"receipt", "operator"},
	"payments c2b-simulate":    {"amount", "msisdn", "account"},
}

func parse(args []string) (command, error) {
	bad := errors.New(usage)
	if len(args) < 2 {
		return command{}, bad
	}
	c := command{name: args[0] + " " + args[1]}
	required, ok := flagsOf[c.name]
	if !ok {
		return command{}, bad
	}
	fs := flag.NewFlagSet(c.name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&c.receipt, "receipt", "", "M-Pesa receipt")
	fs.StringVar(&c.operator, "operator", "", "who is acting (audited)")
	fs.StringVar(&c.reason, "reason", "", "why the top-up was reversed")
	fs.StringVar(&c.payment, "payment", "", "payment id")
	fs.StringVar(&c.user, "user", "", "player (user id) to credit")
	fs.StringVar(&c.refundRef, "refund-ref", "", "M-Pesa reference of the refund")
	fs.StringVar(&c.msisdn, "msisdn", "", "payer number (sandbox)")
	fs.StringVar(&c.account, "account", "", "account number typed by the payer")
	fs.Int64Var(&c.amount, "amount", 0, "amount, whole KES")
	if err := fs.Parse(args[2:]); err != nil || fs.NArg() != 0 {
		return command{}, bad
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = f.Value.String() != "" && f.Value.String() != "0" })
	for _, name := range required {
		if !set[name] {
			return command{}, bad
		}
		delete(set, name)
	}
	if len(set) > 0 || c.amount < 0 {
		return command{}, bad
	}
	if c.operator != "" {
		c.operator = "operator:" + c.operator
	}
	return c, nil
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

	switch cmd.name {
	case "payments c2b-register", "payments c2b-simulate":
		return paybill(ctx, cfg, cmd, out)
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
	case "payments suspense-credit":
		id, err := svc.ResolveSuspense(ctx, cmd.receipt, cmd.user, cmd.operator)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, "credited suspense receipt", cmd.receipt, "as payment", id)
		return err
	case "payments suspense-refund":
		if err := svc.RefundSuspense(ctx, cmd.receipt, cmd.refundRef, cmd.operator); err != nil {
			return err
		}
		_, err := fmt.Fprintln(out, "recorded refund of suspense receipt", cmd.receipt)
		return err
	case "payments c2b-recheck":
		if err := svc.RecheckC2B(ctx, cmd.receipt, cmd.operator); err != nil {
			return err
		}
		_, err := fmt.Fprintln(out, "paybill payment", cmd.receipt, "will be verified again by the worker")
		return err
	}
	return fmt.Errorf("unknown command %q", cmd.name)
}

// paybill runs the commands that talk to Daraja: they need the api's payments configuration with C2B.
func paybill(ctx context.Context, cfg config.Config, cmd command, out io.Writer) error {
	if !cfg.Payments.C2BEnabled() {
		return errors.New("paybill payments are not configured (DARAJA_* and DARAJA_C2B_SHORTCODE, DARAJA_INITIATOR_NAME, DARAJA_SECURITY_CREDENTIAL)")
	}
	client, err := app.NewDaraja(cfg.Payments)
	if err != nil {
		return err
	}
	if cmd.name == "payments c2b-simulate" {
		if err := client.SimulateC2B(ctx, cmd.amount, cmd.msisdn, cmd.account); err != nil {
			return err
		}
		_, err := fmt.Fprintln(out, "simulated a paybill payment; Daraja will call the confirmation URL")
		return err
	}
	// Cancel when our validation URL cannot be reached: a payment we could not check is refused, not taken.
	v, c := cfg.Payments.C2BURL("validate"), cfg.Payments.C2BURL("confirm")
	if err := client.RegisterC2BURLs(ctx, v, c, daraja.C2BCancelOnTimeout); err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, "registered the paybill URLs for shortcode", cfg.Payments.C2BShortCode)
	return err
}
