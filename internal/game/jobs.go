package game

import (
	"context"
	"time"

	"github.com/riverqueue/river"
)

// ReconcileEvery is how often the game reconciliation runs.
const ReconcileEvery = 24 * time.Hour

// ReconcileArgs runs the daily game reconciliation.
type ReconcileArgs struct{}

// Kind implements river.JobArgs.
func (ReconcileArgs) Kind() string { return "game.reconcile" }

type reconcileWorker struct {
	river.WorkerDefaults[ReconcileArgs]
	svc *Service
}

func (w *reconcileWorker) Work(ctx context.Context, _ *river.Job[ReconcileArgs]) error {
	_, err := w.svc.Reconcile(ctx)
	return err
}

// AddWorkers registers the game's job workers.
func AddWorkers(workers *river.Workers, svc *Service) {
	river.AddWorker(workers, &reconcileWorker{svc: svc})
}

// PeriodicJobs are the scheduled game jobs.
func PeriodicJobs() []*river.PeriodicJob {
	return []*river.PeriodicJob{
		river.NewPeriodicJob(river.PeriodicInterval(ReconcileEvery),
			func() (river.JobArgs, *river.InsertOpts) { return ReconcileArgs{}, nil }, &river.PeriodicJobOpts{ID: "game.reconcile", RunOnStart: true}),
	}
}
