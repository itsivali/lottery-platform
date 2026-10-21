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
	if cfg.MetricsAddr == "off" {
		cfg.MetricsAddr = ""
	}
	if cfg.DatabaseURL == "" {
		return Config{}, errors.New("config: DATABASE_URL is required")
	}
	return cfg, nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
