package config_test

import (
	"bytes"
	"encoding/base64"
	"os"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/itsivali/lottery-platform/internal/platform/config"
)

// The Blueprint must supply everything the API needs to start in full mode, and must make the trusted-proxy decision
// explicitly. If config grows a required variable, this fails until render.yaml provides it.
func TestRenderBlueprint_ProvidesEveryVariableTheAPIRequires(t *testing.T) {
	raw, err := os.ReadFile("../../../render.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var bp struct {
		Services []struct {
			Name    string `yaml:"name"`
			Type    string `yaml:"type"`
			EnvVars []struct {
				Key   string `yaml:"key"`
				Value string `yaml:"value"`
			} `yaml:"envVars"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(raw, &bp); err != nil {
		t.Fatal(err)
	}
	var api map[string]string
	var kinds = map[string]string{}
	for _, s := range bp.Services {
		kinds[s.Name] = s.Type
		if s.Type == "web" {
			api = map[string]string{}
			for _, e := range s.EnvVars {
				api[e.Key] = e.Value
			}
		}
	}
	if api == nil {
		t.Fatal("no web service in render.yaml")
	}
	for _, k := range []string{"AUTH_KEY", "TOKEN_SEED", "DATA_KEY", "IDENTITY_KEY", "VALKEY_ADDR", "TRUSTED_PROXY_HEADER", "DATABASE_URL"} {
		if _, ok := api[k]; !ok {
			t.Errorf("web service does not set %s", k)
		}
	}
	if api["TRUSTED_PROXY_HEADER"] == "" || api["TRUSTED_PROXY_HEADER"] == "none" {
		t.Errorf("TRUSTED_PROXY_HEADER = %q: behind Render's load balancer it must name a real header", api["TRUSTED_PROXY_HEADER"])
	}
	if api["WORKERS_IN_PROCESS"] != "true" || api["MIGRATE_ON_START"] != "true" {
		t.Errorf("free plan has no worker or pre-deploy command: WORKERS_IN_PROCESS=%q MIGRATE_ON_START=%q", api["WORKERS_IN_PROCESS"], api["MIGRATE_ON_START"])
	}
	if kinds["lottery-cache"] != "keyvalue" {
		t.Errorf("no key-value instance for rate limits: %v", kinds)
	}
}

// A fresh Blueprint deploy has the plain and generated values and nothing that is entered in the dashboard. The api
// must start from exactly that (it did not: a generated DARAJA_CALLBACK_TOKEN alone counted as half-configured
// payments and the deploy of #27 failed).
func TestRenderBlueprint_AFreshDeployStarts(t *testing.T) {
	raw, err := os.ReadFile("../../../render.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var bp struct {
		Services []struct {
			Type    string `yaml:"type"`
			EnvVars []struct {
				Key           string `yaml:"key"`
				Value         string `yaml:"value"`
				GenerateValue bool   `yaml:"generateValue"`
				FromDatabase  any    `yaml:"fromDatabase"`
				FromService   any    `yaml:"fromService"`
			} `yaml:"envVars"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(raw, &bp); err != nil {
		t.Fatal(err)
	}
	for _, s := range bp.Services {
		if s.Type != "web" {
			continue
		}
		for _, e := range s.EnvVars {
			switch {
			case e.GenerateValue: // Render generates a random base64-encoded 256-bit value
				t.Setenv(e.Key, base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xfb}, 32)))
			case e.FromDatabase != nil:
				t.Setenv(e.Key, "postgres://render")
			case e.FromService != nil:
				t.Setenv(e.Key, "redis://render:6379")
			default: // a plain value, or sync: false (empty until entered in the dashboard)
				t.Setenv(e.Key, e.Value)
			}
		}
	}
	t.Setenv("RENDER_EXTERNAL_URL", "https://lottery-api.onrender.com")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("a fresh Blueprint deploy does not start: %v", err)
	}
	if !cfg.API.Enabled || cfg.Payments.Enabled {
		t.Fatalf("api=%v payments=%v; want the API on and payments off until the Daraja credentials are entered", cfg.API.Enabled, cfg.Payments.Enabled)
	}
}
