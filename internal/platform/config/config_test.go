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
