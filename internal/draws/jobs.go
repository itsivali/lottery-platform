package draws

import (
	"context"
	"time"

	"github.com/riverqueue/river"
)

// ScheduleEvery is how often the draw scheduler looks for due steps.
const ScheduleEvery = 5 * time.Minute

// ScheduleArgs runs the draw scheduler once.
type ScheduleArgs struct{}

// Kind implements river.JobArgs.
func (ScheduleArgs) Kind() string { return "draws.schedule" }

type scheduleWorker struct {
	river.WorkerDefaults[ScheduleArgs]
	svc *Service
}

func (w *scheduleWorker) Work(ctx context.Context, _ *river.Job[ScheduleArgs]) error {
	return w.svc.Schedule(ctx)
}

// AddWorkers registers the draw workers.
func AddWorkers(workers *river.Workers, svc *Service) {
	river.AddWorker(workers, &scheduleWorker{svc: svc})
}

// PeriodicJobs are the scheduled draw jobs.
func PeriodicJobs() []*river.PeriodicJob {
	return []*river.PeriodicJob{
		river.NewPeriodicJob(river.PeriodicInterval(ScheduleEvery),
			func() (river.JobArgs, *river.InsertOpts) { return ScheduleArgs{}, &river.InsertOpts{MaxAttempts: 1} },
			&river.PeriodicJobOpts{ID: "draws.schedule", RunOnStart: true}),
	}
}
