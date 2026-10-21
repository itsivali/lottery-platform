// Package config loads process configuration from the environment.
package config

import (
	"errors"
	"os"
)

// Config is the runtime configuration shared by the binaries.
type Config struct {
	HTTPAddr    string
	DatabaseURL string
	// OTLPEndpoint is the OTLP/HTTP traces endpoint (host:port); empty disables trace export.
	OTLPEndpoint string
	// MetricsAddr is the internal Prometheus listener; empty disables it. Never the public port.
	MetricsAddr string
	// WorkersInProcess runs the background job workers inside the api process (single-service deployments such as the
	// Render free plan). A dedicated worker process (cmd/worker) is preferred when it can be afforded.
	WorkersInProcess bool
	// MigrateOnStart applies pending migrations when the api starts (for hosts without a pre-deploy command).
	MigrateOnStart bool
	// API configures the public /v1 API (disabled unless its secrets are present).
	API API
}

// Load reads configuration from environment variables.
func Load() (Config, error) {
	addr := getenv("HTTP_ADDR", "")
	if addr == "" {
		// Hosting platforms such as Render provide the listen port as PORT.
		if port := os.Getenv("PORT"); port != "" {
			addr = ":" + port
		} else {
			addr = ":8080"
		}
	}
	cfg := Config{
		HTTPAddr:     addr,
		DatabaseURL:  os.Getenv("DATABASE_URL"),
		OTLPEndpoint: os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
		MetricsAddr:  getenv("METRICS_ADDR", "127.0.0.1:9464"),
	}
	cfg.WorkersInProcess = os.Getenv("WORKERS_IN_PROCESS") == "true"
	cfg.MigrateOnStart = os.Getenv("MIGRATE_ON_START") == "true"
	if cfg.MetricsAddr == "off" {
		cfg.MetricsAddr = ""
	}
	if cfg.DatabaseURL == "" {
		return Config{}, errors.New("config: DATABASE_URL is required")
	}
	api, err := loadAPI()
	if err != nil {
		return Config{}, err
	}
	cfg.API = api
	return cfg, nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
