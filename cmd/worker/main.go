// Command worker is the background-job binary. River jobs arrive in later phases;
// until then it only verifies its database and idles, so it refuses (closed) rather than doing nothing silently.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/itsivali/lottery-platform/internal/platform/config"
	"github.com/itsivali/lottery-platform/internal/platform/db"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := config.Load()
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Error("db", "err", err)
		os.Exit(1)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		log.Error("db ping", "err", err)
		os.Exit(1)
	}
	log.Info("worker started; no jobs registered yet")
	<-ctx.Done()
}
