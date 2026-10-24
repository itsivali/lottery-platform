// Command lotteryctl is the admin CLI: migrations and the payments operator actions (reverse a top-up M-Pesa took
// back, resolve an unconfirmed top-up from the statement, credit or refund a paybill payment held in suspense, check an
// unverified paybill payment again, run the internal reconciliation) and manual KYC review. It needs DATABASE_URL; operator actions are
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
	"net/http"
	"os"
	"time"

	"github.com/itsivali/lottery-platform/internal/app"
	"github.com/itsivali/lottery-platform/internal/audit"
	"github.com/itsivali/lottery-platform/internal/draws"
	"github.com/itsivali/lottery-platform/internal/draws/drand"
	"github.com/itsivali/lottery-platform/internal/game"
	"github.com/itsivali/lottery-platform/internal/identity"
	"github.com/itsivali/lottery-platform/internal/kyc"
	"github.com/itsivali/lottery-platform/internal/ledger"
	"github.com/itsivali/lottery-platform/internal/payments"
	"github.com/itsivali/lottery-platform/internal/payments/daraja"
	"github.com/itsivali/lottery-platform/internal/platform/clock"
	"github.com/itsivali/lottery-platform/internal/platform/config"
	"github.com/itsivali/lottery-platform/internal/platform/crypto"
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
  lotteryctl payments c2b-simulate -amount KES -msisdn 2547XXXXXXXX -account ACCOUNT
  lotteryctl kyc pending -operator NAME
  lotteryctl kyc approve|reject -user ID -reason TEXT -operator NAME
  lotteryctl game reconcile
  lotteryctl game simulate [-days N] [-slots N] [-lucky N] [-price KES]   (no database needed)
  lotteryctl draws list
  lotteryctl draws show -draw ID
  lotteryctl draws sign|approve -draw ID -operator NAME
  lotteryctl draws void -draw ID -reason TEXT -operator NAME`

type command struct {
	name                               string
	receipt, reason, operator, payment string
	user, refundRef, msisdn, account   string
	draw                               string
	amount                             int64
	days, slots, lucky                 int
	price                              int64
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
	"kyc pending":              {"operator"},
	"kyc approve":              {"user", "reason", "operator"},
	"kyc reject":               {"user", "reason", "operator"},
	"game reconcile":           nil,
	"game simulate":            nil,
	"draws list":               nil,
	"draws show":               {"draw"},
	"draws sign":               {"draw", "operator"},
	"draws approve":            {"draw", "operator"},
	"draws void":               {"draw", "reason", "operator"},
}

// optionalOf lists, per command, the flags it may take; the rest of a command's flags are as in flagsOf.
var optionalOf = map[string][]string{
	"game simulate": {"days", "slots", "lucky", "price"},
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
	fs.StringVar(&c.draw, "draw", "", "draw id")
	fs.Int64Var(&c.amount, "amount", 0, "amount, whole KES")
	fs.IntVar(&c.days, "days", 0, "simulated player-days per number of plays")
	fs.IntVar(&c.slots, "slots", 0, "slot count")
	fs.IntVar(&c.lucky, "lucky", 0, "lucky slots")
	fs.Int64Var(&c.price, "price", 0, "price of a play, whole KES")
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
	for _, name := range optionalOf[c.name] {
		delete(set, name)
	}
	if len(set) > 0 || c.amount < 0 || c.days < 0 || c.slots < 0 || c.lucky < 0 || c.price < 0 {
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
	if cmd.name == "game simulate" {
		return simulate(cmd, out)
	}
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
	case "kyc pending", "kyc approve", "kyc reject":
		return kycReview(ctx, cfg, cmd, out)
	case "game reconcile":
		return gameReconcile(ctx, cfg, out)
	case "draws list", "draws show", "draws sign", "draws approve", "draws void":
		return drawsCmd(ctx, cfg, cmd, out)
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

// kycReview lists submissions waiting for a staff decision or records one (ADR 0009: identities are checked by hand).
// Listing decrypts personal data, so it needs the api's DATA_KEY (and the rest of its configuration) and is audited.
func kycReview(ctx context.Context, cfg config.Config, cmd command, out io.Writer) error {
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	var kr *crypto.Keyring
	if cmd.name == "kyc pending" {
		if !cfg.API.Enabled {
			return errors.New("listing submissions decrypts them: set the api's configuration (DATA_KEY and the other api secrets)")
		}
		if kr, err = crypto.NewKeyring(cfg.API.DataKeyActive, cfg.API.DataKeys); err != nil {
			return err
		}
	}
	svc := kyc.NewService(pool, clock.Postgres{}, kyc.ManualProvider{}, kr, cfg.API.IdentityKey, audit.NewWriter(), nil)
	if cmd.name == "kyc pending" {
		list, err := svc.AwaitingReview(ctx, cmd.operator)
		if err != nil {
			return err
		}
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(list)
	}
	st, err := svc.Review(ctx, cmd.user, cmd.operator, cmd.name == "kyc approve", cmd.reason)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, "user", cmd.user, "is now", st)
	return err
}

// gameReconcile runs the game reconciliation (stakes, tickets, spins, seed chains); it needs only the database.
func gameReconcile(ctx context.Context, cfg config.Config, out io.Writer) error {
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	svc := game.NewService(pool, clock.Postgres{}, nil, nil, nil, nil, nil)
	r, err := svc.Reconcile(ctx)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(r); err != nil {
		return err
	}
	if !r.OK {
		return errors.New("game reconciliation found mismatches")
	}
	return nil
}

// simulate reports how often a day is lucky and what a draw chance costs under an odds version, from real fair spins.
// Unset flags take the odds version in force (placeholders until the owner sets them).
func simulate(cmd command, out io.Writer) error {
	def := func(v, d int) int {
		if v == 0 {
			return d
		}
		return v
	}
	if cmd.price == 0 {
		cmd.price = 100
	}
	cmd.days, cmd.slots, cmd.lucky = def(cmd.days, 100_000), def(cmd.slots, 12), def(cmd.lucky, 1)
	rep, err := game.Simulate(game.SimConfig{Slots: cmd.slots, LuckySlots: cmd.lucky, PriceKES: cmd.price,
		Tiers: map[int]int{1: 1, 2: 2, 3: 3, 4: 5}, Days: cmd.days})
	if err != nil {
		return err
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(rep)
}

// drawsCmd shows draws and records officials' sign-offs (two different officials sign the frozen entry list before the
// drand round and approve the result). Voiding re-runs the draw with a new company seed, which is encrypted with the
// api's DATA_KEY, so it needs the api's configuration; the other commands need only the database.
func drawsCmd(ctx context.Context, cfg config.Config, cmd command, out io.Writer) error {
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	var kr *crypto.Keyring
	if cmd.name == "draws void" {
		if !cfg.API.Enabled {
			return errors.New("voiding re-runs the draw with a new encrypted seed: set the api's configuration (DATA_KEY and the other api secrets)")
		}
		if kr, err = crypto.NewKeyring(cfg.API.DataKeyActive, cfg.API.DataKeys); err != nil {
			return err
		}
	}
	svc := draws.NewService(pool, clock.Postgres{}, kr, drand.New(drand.Quicknet, drand.DefaultRelays, &http.Client{Timeout: 20 * time.Second}), ledger.NewStore())
	var res any
	switch cmd.name {
	case "draws list":
		res, err = svc.List(ctx, 40)
	case "draws show":
		res, err = svc.Get(ctx, cmd.draw)
	case "draws sign":
		res, err = svc.Sign(ctx, cmd.draw, cmd.operator)
	case "draws approve":
		res, err = svc.Approve(ctx, cmd.draw, cmd.operator)
	case "draws void":
		res, err = svc.Void(ctx, cmd.draw, cmd.operator, cmd.reason)
	}
	if err != nil {
		return err
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(res)
}
