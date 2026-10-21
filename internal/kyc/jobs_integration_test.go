//go:build integration

package kyc_test

import (
	"context"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/itsivali/lottery-platform/internal/audit"
	"github.com/itsivali/lottery-platform/internal/kyc"
	"github.com/itsivali/lottery-platform/internal/platform/clock"
	"github.com/itsivali/lottery-platform/internal/platform/crypto"
	"github.com/itsivali/lottery-platform/internal/platform/testpg"
)

// riverEnv wires the real queue: the api role only enqueues, the worker role runs jobs.
type riverEnv struct {
	*env
	api    *pgxpool.Pool
	worker *pgxpool.Pool
}

func rolePool(t *testing.T, dsn, role string) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, "SET ROLE "+role)
		return err
	}
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

func setupRiver(t *testing.T, maxAttempts int) *riverEnv {
	t.Helper()
	dsn := testpg.Migrated(t)
	api, worker := rolePool(t, dsn, "app_api"), rolePool(t, dsn, "app_worker")
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	kr, _ := crypto.NewKeyring(1, map[byte][]byte{1: k})
	prov := kyc.NewMock()
	clk := clock.NewManual(time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC))
	idKey := []byte("0123456789abcdef0123456789abcdef")

	insertOnly, err := river.NewClient(riverpgxv5.New(api), &river.Config{Schema: "river"})
	if err != nil {
		t.Fatal(err)
	}
	apiSvc := kyc.NewService(api, clk, prov, kr, idKey, audit.NewWriter(), kyc.NewRiverEnqueuer(insertOnly, maxAttempts))
	workerSvc := kyc.NewService(worker, clk, prov, kr, idKey, audit.NewWriter(), nil)

	workers := river.NewWorkers()
	kyc.AddWorkers(workers, workerSvc)
	wc, err := river.NewClient(riverpgxv5.New(worker), &river.Config{
		Schema: "river", Workers: workers, Queues: map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 4}},
		FetchPollInterval: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := wc.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wc.Stop(context.Background()) })

	e := &env{pool: api, clk: clk, prov: prov, kr: kr, svc: apiSvc}
	return &riverEnv{env: e, api: api, worker: worker}
}

func (r *riverEnv) waitStatus(t *testing.T, user string, want kyc.Status) {
	t.Helper()
	deadline := time.Now().Add(40 * time.Second) //nolint:forbidigo // test polling deadline
	for {
		st, err := r.svc.Status(context.Background(), user)
		if err != nil {
			t.Fatal(err)
		}
		if st == want {
			return
		}
		if time.Now().After(deadline) { //nolint:forbidigo // test polling deadline
			t.Fatalf("status stayed %q, want %q", st, want)
		}
		time.Sleep(100 * time.Millisecond) //nolint:forbidigo // polling helper
	}
}

func TestRiver_SubmissionIsVerifiedByTheWorker(t *testing.T) {
	r := setupRiver(t, 8)
	r.registerJane()
	u := r.user(t)
	if st, err := r.svc.Submit(context.Background(), u, jane); err != nil || st != kyc.Pending {
		t.Fatalf("submit: %q %v", st, err)
	}
	r.waitStatus(t, u, kyc.Verified)
}

func TestRiver_ProviderOutageIsRetriedUntilItRecovers(t *testing.T) {
	r := setupRiver(t, 8)
	r.registerJane()
	u := r.user(t)
	r.prov.FailNext(errors.New("timeout")) // first attempt fails, the retry succeeds
	if _, err := r.svc.Submit(context.Background(), u, jane); err != nil {
		t.Fatal(err)
	}
	r.waitStatus(t, u, kyc.Verified)
}

func TestRiver_ExhaustedRetriesFreeThePlayerToResubmit(t *testing.T) {
	r := setupRiver(t, 2)
	r.registerJane()
	u := r.user(t)
	r.prov.SetDown(errors.New("registry down"))
	if _, err := r.svc.Submit(context.Background(), u, jane); err != nil {
		t.Fatal(err)
	}
	r.waitStatus(t, u, kyc.None)
	r.prov.SetDown(nil)
	if _, err := r.svc.Submit(context.Background(), u, jane); err != nil {
		t.Fatalf("resubmission after abandonment: %v", err)
	}
	r.waitStatus(t, u, kyc.Verified)
}

func TestRiver_ARolledBackSubmissionLeavesNoJob(t *testing.T) {
	r := setupRiver(t, 8)
	ctx := context.Background()
	enq := kyc.NewRiverEnqueuer(mustInsertOnly(t, r.api), 8)
	err := pgx.BeginFunc(ctx, r.api, func(tx pgx.Tx) error {
		if err := enq.EnqueueVerify(ctx, tx, "00000000-0000-7000-8000-000000000001", "00000000-0000-7000-8000-000000000002"); err != nil {
			return err
		}
		return errors.New("business step failed after enqueue")
	})
	if err == nil {
		t.Fatal("expected the transaction to fail")
	}
	var n int
	_ = r.api.QueryRow(ctx, `SELECT count(*) FROM river.river_job WHERE kind = 'kyc.verify'`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d jobs survived a rolled-back transaction", n)
	}
}

func mustInsertOnly(t *testing.T, p *pgxpool.Pool) *river.Client[pgx.Tx] {
	t.Helper()
	c, err := river.NewClient(riverpgxv5.New(p), &river.Config{Schema: "river"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}
