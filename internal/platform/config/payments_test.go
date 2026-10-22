package config_test

import (
	"strings"
	"testing"

	"github.com/itsivali/lottery-platform/internal/platform/config"
)

var darajaVars = []string{"DARAJA_CONSUMER_KEY", "DARAJA_CONSUMER_SECRET", "DARAJA_SHORTCODE", "DARAJA_PASSKEY", "DARAJA_CALLBACK_TOKEN"}

func fullPaymentsEnv(t *testing.T) {
	t.Helper()
	fullAPIEnv(t)
	t.Setenv("DARAJA_CONSUMER_KEY", "ck")
	t.Setenv("DARAJA_CONSUMER_SECRET", "cs")
	t.Setenv("DARAJA_SHORTCODE", "174379")
	t.Setenv("DARAJA_PASSKEY", "pk")
	t.Setenv("DARAJA_CALLBACK_TOKEN", "a/b+c=0123456789abcdef0123456789") // Render-generated values are base64
	t.Setenv("DARAJA_BASE_URL", "")
	t.Setenv("DARAJA_ALLOW_PRODUCTION", "")
	t.Setenv("PUBLIC_BASE_URL", "")
	t.Setenv("RENDER_EXTERNAL_URL", "https://lottery-api.onrender.com")
}

func TestPayments_OffWhenNoDarajaCredentialsAreSet(t *testing.T) {
	fullAPIEnv(t)
	for _, k := range darajaVars {
		t.Setenv(k, "")
	}
	cfg, err := config.Load()
	if err != nil || cfg.Payments.Enabled {
		t.Fatalf("enabled=%v err=%v", cfg.Payments.Enabled, err)
	}
}

func TestPayments_AGeneratedCallbackTokenAloneLeavesPaymentsOff(t *testing.T) {
	fullAPIEnv(t)
	for _, k := range darajaVars {
		t.Setenv(k, "")
	}
	t.Setenv("DARAJA_CALLBACK_TOKEN", "generated-by-the-host-0123456789abcdef")
	cfg, err := config.Load()
	if err != nil || cfg.Payments.Enabled {
		t.Fatalf("enabled=%v err=%v; the host generates the token before anyone enters credentials", cfg.Payments.Enabled, err)
	}
}

func TestPayments_EnabledDefaultsToTheSandboxAndAnUnguessableCallbackPath(t *testing.T) {
	fullPaymentsEnv(t)
	cfg, err := config.Load()
	if err != nil || !cfg.Payments.Enabled {
		t.Fatalf("enabled=%v err=%v", cfg.Payments.Enabled, err)
	}
	p := cfg.Payments
	if p.BaseURL != "https://sandbox.safaricom.co.ke" || p.ConsumerKey != "ck" || p.ShortCode != "174379" || p.AllowProduction {
		t.Fatalf("payments = %+v", p)
	}
	if len(p.CallbackPath) != 64 || strings.ContainsAny(p.CallbackPath, "/+=") {
		t.Fatalf("callback path %q must be a hex digest, safe in a URL path", p.CallbackPath)
	}
	if p.CallbackURL() != "https://lottery-api.onrender.com/hooks/mpesa/stk/"+p.CallbackPath {
		t.Fatalf("callback url = %q", p.CallbackURL())
	}
	t.Setenv("PUBLIC_BASE_URL", "https://api.example.ke/")
	cfg, _ = config.Load()
	if !strings.HasPrefix(cfg.Payments.CallbackURL(), "https://api.example.ke/hooks/mpesa/stk/") {
		t.Fatalf("PUBLIC_BASE_URL not preferred: %q", cfg.Payments.CallbackURL())
	}
}

func TestPayments_PartialOrUnsafeConfigurationIsRefused(t *testing.T) {
	for _, missing := range darajaVars {
		fullPaymentsEnv(t)
		t.Setenv(missing, "")
		if _, err := config.Load(); err == nil || !strings.Contains(err.Error(), missing) {
			t.Errorf("missing %s: err = %v", missing, err)
		}
	}
	for name, set := range map[string]func(){
		"short callback token": func() { t.Setenv("DARAJA_CALLBACK_TOKEN", "short") },
		"no public url":        func() { t.Setenv("RENDER_EXTERNAL_URL", "") },
		"production base":      func() { t.Setenv("DARAJA_BASE_URL", "https://api.safaricom.co.ke") },
		"payments without api": func() {
			t.Setenv("AUTH_KEY", "")
			t.Setenv("TOKEN_SEED", "")
			t.Setenv("DATA_KEYS", "")
			t.Setenv("IDENTITY_KEY", "")
			t.Setenv("VALKEY_ADDR", "")
		},
	} {
		fullPaymentsEnv(t)
		set()
		if _, err := config.Load(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	fullPaymentsEnv(t)
	t.Setenv("DARAJA_BASE_URL", "https://api.safaricom.co.ke")
	t.Setenv("DARAJA_ALLOW_PRODUCTION", "true")
	if cfg, err := config.Load(); err != nil || !cfg.Payments.AllowProduction {
		t.Fatalf("explicit production: %v", err)
	}
}
