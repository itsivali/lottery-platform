//go:build integration

package payments_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/itsivali/lottery-platform/internal/audit"
	"github.com/itsivali/lottery-platform/internal/identity"
	"github.com/itsivali/lottery-platform/internal/ledger"
	"github.com/itsivali/lottery-platform/internal/payments"
	"github.com/itsivali/lottery-platform/internal/payments/daraja"
	"github.com/itsivali/lottery-platform/internal/payments/darajamock"
	"github.com/itsivali/lottery-platform/internal/platform/clock"
	"github.com/itsivali/lottery-platform/internal/platform/db"
	"github.com/itsivali/lottery-platform/internal/platform/testpg"
	"github.com/itsivali/lottery-platform/internal/rg"
	"github.com/itsivali/lottery-platform/internal/wallet"
)

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second) //nolint:forbidigo // test polling deadline
	for !ok() {
		if time.Now().After(deadline) { //nolint:forbidigo // test polling deadline
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond) //nolint:forbidigo // polling helper
	}
}

// The job wiring end to end: real River workers push, verify on the callback and credit; the reconcile and sweep
// jobs run from the queue.
func TestJobs_ATopUpIsSettledByTheQueueAlone(t *testing.T) {
	ctx := context.Background()
	pool := testpg.Pool(t, "app_worker")
	clk := clock.Postgres{}
	aw := audit.NewWriter()

	var svc *payments.Service
	cb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if err := svc.HandleCallback(r.Context(), raw); err != nil {
			t.Errorf("callback: %v", err)
		}
	}))
	t.Cleanup(cb.Close)
	mock := darajamock.New(darajamock.Options{ConsumerKey: "ck", ConsumerSecret: "cs", ShortCode: "174379", Passkey: "pk", HTTPClient: cb.Client()})
	mock.AutoComplete(ctx, 0)
	ms := httptest.NewServer(mock)
	t.Cleanup(ms.Close)
	dc, err := daraja.New(daraja.Config{BaseURL: ms.URL, ConsumerKey: "ck", ConsumerSecret: "cs", ShortCode: "174379", Passkey: "pk", CallbackURL: cb.URL},
		ms.Client(), clock.Wall{})
	if err != nil {
		t.Fatal(err)
	}

	insertOnly, err := river.NewClient(riverpgxv5.New(pool), &river.Config{Schema: db.RiverSchema})
	if err != nil {
		t.Fatal(err)
	}
	ls := ledger.NewStore()
	ids := identity.NewService(pool, clk, nil, aw, identity.Config{AuthKey: []byte("0123456789abcdef0123456789abcdef")})
	svc = payments.NewService(pool, clk, dc, payments.NewRiverEnqueuer(insertOnly), ids,
		rg.NewService(pool, clk, aw, rg.Config{CoolingOff: 24 * time.Hour}), wallet.NewService(ls), ls, aw, payments.Config{})

	workers := river.NewWorkers()
	payments.AddWorkers(workers, svc)
	wc, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Schema: db.RiverSchema, Workers: workers, PeriodicJobs: payments.PeriodicJobs(),
		Queues: map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 4}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := wc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wc.Stop(context.Background()) })

	var user, msisdn string = "", "254700000999"
	if err := pool.QueryRow(ctx, `INSERT INTO identity.users (msisdn, kyc_status, national_id_hash, password_hash)
		VALUES ($1, 'verified', 'jobs-test', 'x') RETURNING id::text`, msisdn).Scan(&user); err != nil {
		t.Fatal(err)
	}
	tu, err := svc.StartTopUp(ctx, user, 200, "jobs-key")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the top-up to be credited by the workers", func() bool {
		got, err := svc.Get(ctx, user, tu.ID)
		return err == nil && got.Status == payments.StatusCredited
	})

	runs := func() int {
		var n int
		_ = pool.QueryRow(ctx, `SELECT count(*) FROM payments.reconciliation_runs WHERE ok`).Scan(&n)
		return n
	}
	waitFor(t, "the reconcile job that runs on start", func() bool { return runs() >= 1 })
	before := runs()
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := insertOnly.InsertTx(ctx, tx, payments.ReconcileArgs{}, nil); err != nil {
			return err
		}
		_, err := insertOnly.InsertTx(ctx, tx, payments.SweepArgs{}, nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "an enqueued reconcile job", func() bool { return runs() > before })
	var swept int
	waitFor(t, "the sweep job", func() bool {
		_ = pool.QueryRow(ctx, `SELECT count(*) FROM river.river_job WHERE kind = 'payments.sweep' AND state = 'completed'`).Scan(&swept)
		return swept >= 1
	})
}
