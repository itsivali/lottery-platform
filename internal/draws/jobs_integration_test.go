//go:build integration

package draws

import (
	"context"
	"testing"
	"time"

	"github.com/riverqueue/river"

	"github.com/itsivali/lottery-platform/internal/draws/drand"
	"github.com/itsivali/lottery-platform/internal/ledger"
	"github.com/itsivali/lottery-platform/internal/platform/clock"
	"github.com/itsivali/lottery-platform/internal/platform/testpg"
)

type noBeacon struct{}

func (noBeacon) Chain() drand.Chain { return drand.Quicknet }
func (noBeacon) Fetch(context.Context, time.Time, uint64) (drand.Beacon, error) {
	return drand.Beacon{}, drand.ErrNotYet
}

func TestJobs_TheSchedulerIsPeriodicAndRuns(t *testing.T) {
	jobs := PeriodicJobs()
	if len(jobs) != 1 || (ScheduleArgs{}).Kind() != "draws.schedule" {
		t.Fatalf("periodic jobs %d", len(jobs))
	}
	svc := NewService(testpg.Pool(t, "app_worker"), clock.NewManual(time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)), nil, noBeacon{}, ledger.NewStore())
	AddWorkers(river.NewWorkers(), svc)
	if err := (&scheduleWorker{svc: svc}).Work(context.Background(), &river.Job[ScheduleArgs]{}); err != nil {
		t.Fatal(err)
	}
}
