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
		HTTPAddr:    addr,
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
