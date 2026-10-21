// Command worker runs the background jobs (KYC verification today) in a dedicated process. Single-service
// deployments can instead set WORKERS_IN_PROCESS=true on the api. It needs the same configuration as the api.
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/itsivali/lottery-platform/internal/app"
	"github.com/itsivali/lottery-platform/internal/platform/config"
	"github.com/itsivali/lottery-platform/internal/platform/db"
	"github.com/itsivali/lottery-platform/internal/platform/telemetry"
)

func main() {
	log := slog.New(telemetry.NewLogHandler(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(log); err != nil {
		log.Error("worker exited", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if !cfg.API.Enabled {
		return errors.New("worker: no jobs can run until the API secrets are configured (same variables as the api)")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return err
	}
	application, err := app.New(ctx, cfg, pool)
	if err != nil {
		return err
	}
	defer application.Close(context.Background())
	if err := application.StartWorkers(ctx); err != nil {
		return err
	}
	log.Info("worker started")
	<-ctx.Done()
	return nil
}
