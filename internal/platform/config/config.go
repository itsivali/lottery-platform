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
}

// Load reads configuration from environment variables.
func Load() (Config, error) {
	cfg := Config{
		HTTPAddr:    getenv("HTTP_ADDR", ":8080"),
		DatabaseURL: os.Getenv("DATABASE_URL"),
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
