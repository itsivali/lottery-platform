package config_test

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/itsivali/lottery-platform/internal/platform/config"
)

func b64(n int) string { return base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", n))) }

// fullAPIEnv sets every variable the v1 API needs.
func fullAPIEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("AUTH_KEY", b64(32))
	t.Setenv("TOKEN_SEED", b64(32))
	t.Setenv("DATA_KEYS", "1:"+b64(32))
	t.Setenv("IDENTITY_KEY", b64(32))
	t.Setenv("VALKEY_ADDR", "valkey:6379")
	t.Setenv("TRUSTED_PROXY_HEADER", "X-Forwarded-For")
	for _, k := range []string{"TRUSTED_PROXY_HOPS", "DATA_KEY_ACTIVE", "KYC_API_URL", "KYC_API_KEY", "GOOGLE_CLIENT_ID", "APPLE_CLIENT_ID"} {
		t.Setenv(k, "")
	}
}

func TestAPI_DisabledWhenNoSecretsAreSet(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	for _, k := range []string{"AUTH_KEY", "TOKEN_SEED", "DATA_KEYS", "IDENTITY_KEY", "VALKEY_ADDR", "TRUSTED_PROXY_HEADER"} {
		t.Setenv(k, "")
	}
	cfg, err := config.Load()
	if err != nil || cfg.API.Enabled {
		t.Fatalf("enabled=%v err=%v, want health-only mode", cfg.API.Enabled, err)
	}
}

func TestAPI_EnabledWithEverythingSet(t *testing.T) {
	fullAPIEnv(t)
	cfg, err := config.Load()
	if err != nil || !cfg.API.Enabled {
		t.Fatalf("enabled=%v err=%v", cfg.API.Enabled, err)
	}
	a := cfg.API
	if len(a.AuthKey) != 32 || len(a.TokenSeed) != 32 || len(a.IdentityKey) != 32 || len(a.DataKeys[1]) != 32 || a.DataKeyActive != 1 {
		t.Fatalf("keys not decoded: %+v", a)
	}
	if a.TrustedProxyHeader != "X-Forwarded-For" || a.TrustedProxyHops != 1 || a.ValkeyAddr != "valkey:6379" {
		t.Fatalf("settings: %+v", a)
	}
}

func TestAPI_PartialConfigurationIsRefusedAndNamesWhatIsMissing(t *testing.T) {
	for _, missing := range []string{"AUTH_KEY", "TOKEN_SEED", "DATA_KEYS", "IDENTITY_KEY", "VALKEY_ADDR"} {
		fullAPIEnv(t)
		t.Setenv(missing, "")
		_, err := config.Load()
		if err == nil || !strings.Contains(err.Error(), missing) {
			t.Errorf("missing %s: err = %v, want it to name the variable", missing, err)
		}
	}
}

func TestAPI_TrustedProxyHeaderMustBeChosenExplicitly(t *testing.T) {
	fullAPIEnv(t)
	t.Setenv("TRUSTED_PROXY_HEADER", "")
	if _, err := config.Load(); err == nil || !strings.Contains(err.Error(), "TRUSTED_PROXY_HEADER") {
		t.Fatalf("API started without a proxy decision: %v", err)
	}
	t.Setenv("TRUSTED_PROXY_HEADER", "none")
	cfg, err := config.Load()
	if err != nil || cfg.API.TrustedProxyHeader != "" {
		t.Fatalf("none: header=%q err=%v (none means use the socket address)", cfg.API.TrustedProxyHeader, err)
	}
	t.Setenv("TRUSTED_PROXY_HEADER", "X-Forwarded-For")
	t.Setenv("TRUSTED_PROXY_HOPS", "3")
	if cfg, _ := config.Load(); cfg.API.TrustedProxyHops != 3 {
		t.Fatalf("hops = %d", cfg.API.TrustedProxyHops)
	}
	for _, bad := range []string{"0", "9", "x", "-1"} {
		t.Setenv("TRUSTED_PROXY_HOPS", bad)
		if _, err := config.Load(); err == nil {
			t.Errorf("hops %q accepted", bad)
		}
	}
}

func TestAPI_RejectsMalformedSecrets(t *testing.T) {
	cases := map[string]string{
		"AUTH_KEY":     b64(8),
		"TOKEN_SEED":   b64(16),
		"IDENTITY_KEY": "not base64!!",
		"DATA_KEYS":    "1:" + b64(16),
	}
	for k, v := range cases {
		fullAPIEnv(t)
		t.Setenv(k, v)
		if _, err := config.Load(); err == nil || !strings.Contains(err.Error(), k) {
			t.Errorf("%s=%q: err = %v", k, v, err)
		}
	}
	fullAPIEnv(t)
	t.Setenv("DATA_KEYS", "1:"+b64(32)+",1:"+b64(32))
	if _, err := config.Load(); err == nil {
		t.Error("duplicate key id accepted")
	}
	fullAPIEnv(t)
	t.Setenv("DATA_KEYS", "1:"+b64(32)+",2:"+b64(32))
	t.Setenv("DATA_KEY_ACTIVE", "2")
	if cfg, err := config.Load(); err != nil || cfg.API.DataKeyActive != 2 {
		t.Errorf("rotation: %+v %v", cfg.API.DataKeyActive, err)
	}
	t.Setenv("DATA_KEY_ACTIVE", "7")
	if _, err := config.Load(); err == nil {
		t.Error("active key id not in the keyring accepted")
	}
}

func TestAPI_RegistryURLMustBeHTTPSExceptLocalOnes(t *testing.T) {
	fullAPIEnv(t)
	t.Setenv("KYC_API_URL", "http://registry.example.com/verify")
	if _, err := config.Load(); err == nil || !strings.Contains(err.Error(), "KYC_API_URL") {
		t.Errorf("plain-http public gateway accepted: %v", err)
	}
	for _, ok := range []string{"https://registry.example.com/verify", "http://kyc-mock:8080/verify", "http://localhost:8091/verify"} {
		t.Setenv("KYC_API_URL", ok)
		if _, err := config.Load(); err != nil {
			t.Errorf("%s rejected: %v", ok, err)
		}
	}
}

func TestWorkersInProcess_IsOffUnlessExplicitlyEnabled(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	for _, v := range []string{"", "false", "yes", "1"} {
		t.Setenv("WORKERS_IN_PROCESS", v)
		if cfg, _ := config.Load(); cfg.WorkersInProcess {
			t.Errorf("WORKERS_IN_PROCESS=%q enabled workers", v)
		}
	}
	t.Setenv("WORKERS_IN_PROCESS", "true")
	if cfg, _ := config.Load(); !cfg.WorkersInProcess {
		t.Error("WORKERS_IN_PROCESS=true did not enable workers")
	}
}

func TestAPI_SingleDataKeyShorthand(t *testing.T) {
	fullAPIEnv(t)
	t.Setenv("DATA_KEYS", "")
	t.Setenv("DATA_KEY", b64(32))
	cfg, err := config.Load()
	if err != nil || len(cfg.API.DataKeys[1]) != 32 || cfg.API.DataKeyActive != 1 {
		t.Fatalf("DATA_KEY shorthand: %+v %v", cfg.API.DataKeys, err)
	}
	t.Setenv("DATA_KEY", b64(10))
	if _, err := config.Load(); err == nil {
		t.Error("short DATA_KEY accepted")
	}
}

func TestMigrateOnStart_IsOffUnlessExplicitlyEnabled(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("MIGRATE_ON_START", "")
	if cfg, _ := config.Load(); cfg.MigrateOnStart {
		t.Error("migrations ran by default")
	}
	t.Setenv("MIGRATE_ON_START", "true")
	if cfg, _ := config.Load(); !cfg.MigrateOnStart {
		t.Error("MIGRATE_ON_START=true ignored")
	}
}
