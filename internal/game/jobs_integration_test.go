//go:build integration

package game

import (
	"context"
	"testing"

	"github.com/riverqueue/river"

	"github.com/itsivali/lottery-platform/internal/platform/clock"
	"github.com/itsivali/lottery-platform/internal/platform/testpg"
)

func TestJobs_TheDailyReconciliationIsScheduledAndRuns(t *testing.T) {
	jobs := PeriodicJobs()
	if len(jobs) != 1 || (ReconcileArgs{}).Kind() != "game.reconcile" {
		t.Fatalf("periodic jobs %d", len(jobs))
	}
	workers := river.NewWorkers()
	svc := NewService(testpg.Pool(t, "app_worker"), clock.Postgres{}, nil, nil, nil, nil, nil)
	AddWorkers(workers, svc)
	if err := (&reconcileWorker{svc: svc}).Work(context.Background(), &river.Job[ReconcileArgs]{}); err != nil {
		t.Fatal(err)
	}
}
