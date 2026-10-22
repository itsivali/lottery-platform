package config_test

import (
	"testing"

	"github.com/itsivali/lottery-platform/internal/platform/config"
)

func TestLoad_RequiresDatabaseURL(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	if _, err := config.Load(); err == nil {
		t.Fatal("Load succeeded without DATABASE_URL")
	}
}

func TestLoad_DefaultsAndOverrides(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("HTTP_ADDR", "")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr = %q, want :8080", cfg.HTTPAddr)
	}
	t.Setenv("HTTP_ADDR", ":9090")
	cfg, _ = config.Load()
	if cfg.HTTPAddr != ":9090" {
		t.Errorf("HTTPAddr = %q, want :9090", cfg.HTTPAddr)
	}
}

func TestLoad_UsesRendersPORTWhenHTTPAddrIsUnset(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("HTTP_ADDR", "")
	t.Setenv("PORT", "10000")
	cfg, _ := config.Load()
	if cfg.HTTPAddr != ":10000" {
		t.Errorf("HTTPAddr = %q, want :10000", cfg.HTTPAddr)
	}
	t.Setenv("HTTP_ADDR", "127.0.0.1:7000") // an explicit address wins over PORT
	cfg, _ = config.Load()
	if cfg.HTTPAddr != "127.0.0.1:7000" {
		t.Errorf("HTTPAddr = %q", cfg.HTTPAddr)
	}
}

func TestLoad_TelemetrySettings(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("METRICS_ADDR", "")
	cfg, _ := config.Load()
	if cfg.OTLPEndpoint != "" || cfg.MetricsAddr != "127.0.0.1:9464" {
		t.Errorf("defaults: %+v", cfg)
	}
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "otel.example:4318")
	t.Setenv("METRICS_ADDR", "off")
	cfg, _ = config.Load()
	if cfg.OTLPEndpoint != "otel.example:4318" || cfg.MetricsAddr != "" {
		t.Errorf("overrides: %+v (METRICS_ADDR=off must disable the listener)", cfg)
	}
}

func TestLoad_ClientIPHeaderLoggingIsOffUnlessAskedFor(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("LOG_CLIENT_IP_HEADERS", "")
	if cfg, err := config.Load(); err != nil || cfg.LogClientIPHeaders {
		t.Fatalf("default: %v %v", cfg.LogClientIPHeaders, err)
	}
	t.Setenv("LOG_CLIENT_IP_HEADERS", "true")
	if cfg, err := config.Load(); err != nil || !cfg.LogClientIPHeaders {
		t.Fatalf("enabled: %v %v", cfg.LogClientIPHeaders, err)
	}
}
