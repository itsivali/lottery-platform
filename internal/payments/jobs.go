package payments

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
)

// PushArgs sends the STK prompt for a payment.
type PushArgs struct {
	PaymentID string `json:"payment_id"`
}

// Kind implements river.JobArgs.
func (PushArgs) Kind() string { return "payments.stk_push" }

// VerifyArgs settles a payment from the status query.
type VerifyArgs struct {
	PaymentID string       `json:"payment_id"`
	Source    VerifySource `json:"source"`
}

// Kind implements river.JobArgs.
func (VerifyArgs) Kind() string { return "payments.stk_verify" }

// ReconcileArgs runs the daily internal reconciliation.
type ReconcileArgs struct{}

// Kind implements river.JobArgs.
func (ReconcileArgs) Kind() string { return "payments.reconcile" }

// SweepArgs looks for payments that have been pending too long.
type SweepArgs struct{}

// Kind implements river.JobArgs.
func (SweepArgs) Kind() string { return "payments.sweep" }

const verifyMaxAttempts = 20

// RiverEnqueuer enqueues payment jobs inside the caller's transaction.
type RiverEnqueuer struct{ client *river.Client[pgx.Tx] }

// NewRiverEnqueuer returns an Enqueuer on an (insert-only is enough) River client.
func NewRiverEnqueuer(c *river.Client[pgx.Tx]) *RiverEnqueuer { return &RiverEnqueuer{client: c} }

// EnqueuePush implements Enqueuer. The push job runs exactly once (see Service.Push).
func (e *RiverEnqueuer) EnqueuePush(ctx context.Context, tx pgx.Tx, paymentID string) error {
	_, err := e.client.InsertTx(ctx, tx, PushArgs{PaymentID: paymentID}, &river.InsertOpts{MaxAttempts: 1})
	return err
}

// EnqueueVerify implements Enqueuer.
func (e *RiverEnqueuer) EnqueueVerify(ctx context.Context, tx pgx.Tx, paymentID string, source VerifySource) error {
	_, err := e.client.InsertTx(ctx, tx, VerifyArgs{PaymentID: paymentID, Source: source},
		&river.InsertOpts{MaxAttempts: verifyMaxAttempts, UniqueOpts: river.UniqueOpts{ByArgs: true}})
	return err
}

type pushWorker struct {
	river.WorkerDefaults[PushArgs]
	svc *Service
}

func (w *pushWorker) Work(ctx context.Context, j *river.Job[PushArgs]) error {
	return w.svc.Push(ctx, j.Args.PaymentID)
}

type verifyWorker struct {
	river.WorkerDefaults[VerifyArgs]
	svc *Service
}

func (w *verifyWorker) Work(ctx context.Context, j *river.Job[VerifyArgs]) error {
	d, err := w.svc.Verify(ctx, j.Args.PaymentID)
	if err != nil {
		return err
	}
	if d > 0 {
		return river.JobSnooze(d)
	}
	return nil
}

type sweepWorker struct {
	river.WorkerDefaults[SweepArgs]
	svc *Service
}

func (w *sweepWorker) Work(ctx context.Context, _ *river.Job[SweepArgs]) error {
	return w.svc.Sweep(ctx)
}

type reconcileWorker struct {
	river.WorkerDefaults[ReconcileArgs]
	svc *Service
}

func (w *reconcileWorker) Work(ctx context.Context, _ *river.Job[ReconcileArgs]) error {
	_, err := w.svc.Reconcile(ctx)
	return err
}

// SweepEvery is how often the sweeper looks for stuck payments.
const SweepEvery = time.Minute

// AddWorkers registers the payment job workers.
func AddWorkers(ws *river.Workers, svc *Service) {
	river.AddWorker(ws, &pushWorker{svc: svc})
	river.AddWorker(ws, &verifyWorker{svc: svc})
	river.AddWorker(ws, &sweepWorker{svc: svc})
	river.AddWorker(ws, &reconcileWorker{svc: svc})
}

// ReconcileEvery is how often the internal reconciliation runs.
const ReconcileEvery = 24 * time.Hour

// PeriodicJobs are the scheduled payment jobs.
func PeriodicJobs() []*river.PeriodicJob {
	return []*river.PeriodicJob{
		river.NewPeriodicJob(river.PeriodicInterval(SweepEvery),
			func() (river.JobArgs, *river.InsertOpts) { return SweepArgs{}, nil }, &river.PeriodicJobOpts{ID: "payments.sweep"}),
		river.NewPeriodicJob(river.PeriodicInterval(ReconcileEvery),
			func() (river.JobArgs, *river.InsertOpts) { return ReconcileArgs{}, nil }, &river.PeriodicJobOpts{ID: "payments.reconcile", RunOnStart: true}),
	}
}
