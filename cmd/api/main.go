// Command api is the HTTP binary.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/itsivali/lottery-platform/internal/app"
	"github.com/itsivali/lottery-platform/internal/platform/config"
	"github.com/itsivali/lottery-platform/internal/platform/db"
	"github.com/itsivali/lottery-platform/internal/platform/httpx"
	"github.com/itsivali/lottery-platform/internal/platform/telemetry"
)

func main() {
	log := slog.New(telemetry.NewLogHandler(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(log); err != nil {
		log.Error("api exited", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cfg.MigrateOnStart {
		log.Info("applying migrations")
		if err := db.MigrateUpExclusive(ctx, cfg.DatabaseURL); err != nil {
			return err
		}
	}
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	application, err := app.New(ctx, cfg, pool)
	if err != nil {
		return err
	}
	defer application.Close(context.Background())
	if application.Mount == nil {
		log.Warn("v1 API not configured: serving health probes only (set OTP_KEY, TOKEN_SEED, DATA_KEY(S), IDENTITY_KEY, VALKEY_ADDR and TRUSTED_PROXY_HEADER to enable it)")
	}
	if cfg.WorkersInProcess {
		if err := application.StartWorkers(ctx); err != nil {
			return err
		}
	}

	tel, err := telemetry.New(ctx, telemetry.Config{ServiceName: "lottery-api", OTLPEndpoint: cfg.OTLPEndpoint})
	if err != nil {
		return err
	}
	defer func() { _ = tel.Shutdown(context.Background()) }()
	if cfg.MetricsAddr != "" {
		go serveMetrics(log, cfg.MetricsAddr, tel)
	}

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           tel.Middleware(httpx.NewRouterWith(pool, application.Mount)),
		ReadHeaderTimeout: 5 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("api listening", "addr", cfg.HTTPAddr)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}

// serveMetrics exposes Prometheus metrics on an internal-only listener, separate from the public port.
func serveMetrics(log *slog.Logger, addr string, tel *telemetry.Telemetry) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", tel.MetricsHandler())
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("metrics listener stopped", "err", err)
	}
}
