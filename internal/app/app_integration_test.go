//go:build integration

package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/itsivali/lottery-platform/internal/app"
	"github.com/itsivali/lottery-platform/internal/platform/config"
	"github.com/itsivali/lottery-platform/internal/platform/httpx"
	"github.com/itsivali/lottery-platform/internal/platform/testpg"
	"github.com/itsivali/lottery-platform/internal/platform/testvalkey"
)

// gateways is a local stand-in for the national-registry vendor.
type gateways struct{ registry *httptest.Server }

func newGateways(t *testing.T) *gateways {
	t.Helper()
	g := &gateways{}
	g.registry = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		ok := in["id_number"] == "12345678" && in["full_name"] == "Jane Wanjiru Kamau" && in["date_of_birth"] == "1990-05-17"
		_ = json.NewEncoder(w).Encode(map[string]any{"found": in["id_number"] == "12345678", "name_match": ok, "dob_match": ok, "reference": "r1"})
	}))
	t.Cleanup(g.registry.Close)
	return g
}

func baseCfg(t *testing.T, g *gateways, vk *testvalkey.Server) config.Config {
	t.Helper()
	return config.Config{
		DatabaseURL: "unused", WorkersInProcess: true,
		API: config.API{
			Enabled: true, AuthKey: bytes.Repeat([]byte("o"), 32), TokenSeed: bytes.Repeat([]byte("t"), 32), IdentityKey: bytes.Repeat([]byte("i"), 32),
			DataKeys: map[byte][]byte{1: bytes.Repeat([]byte("d"), 32)}, DataKeyActive: 1, ValkeyAddr: vk.Addr,
			TrustedProxyHeader: "X-Forwarded-For", TrustedProxyHops: 1,
			KYCURL: g.registry.URL, KYCKey: "kyc-key",
		},
	}
}

func call(t *testing.T, srv *httptest.Server, method, path, token, key, xff string, body any) (int, map[string]any) {
	t.Helper()
	var rd io.Reader = http.NoBody
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequestWithContext(context.Background(), method, srv.URL+path, rd)
	req.Header.Set("Idempotency-Key", key)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Body.Close() }()
	var out map[string]any
	_ = json.NewDecoder(r.Body).Decode(&out)
	return r.StatusCode, out
}

func TestApp_FullJourneyThroughTheRealWiring(t *testing.T) {
	ctx := context.Background()
	g := newGateways(t)
	vk := testvalkey.Start(t)
	pool := testpg.Pool(t, "app_worker") // one role for api and worker, as in a single-service deployment
	a, err := app.New(ctx, baseCfg(t, g, vk), pool)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close(context.Background()) })
	if a.Mount == nil {
		t.Fatal("API did not mount although fully configured")
	}
	if err := a.StartWorkers(ctx); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(httpx.NewRouterWith(pool, a.Mount))
	t.Cleanup(srv.Close)

	dev := map[string]any{"fingerprint": "fp", "platform": "android"}
	if c, out := call(t, srv, http.MethodPost, "/v1/auth/signup", "", "k1", "198.51.100.1",
		map[string]any{"msisdn": "0712345678", "password": "correct horse battery", "device": dev}); c != 201 {
		t.Fatalf("signup = %d %v", c, out)
	}
	c, tok := call(t, srv, http.MethodPost, "/v1/auth/login", "", "k2", "198.51.100.1",
		map[string]any{"msisdn": "0712345678", "password": "correct horse battery", "device": dev})
	if c != 200 {
		t.Fatalf("login = %d %v", c, tok)
	}
	access := tok["access_token"].(string)

	if c, out := call(t, srv, http.MethodPost, "/v1/kyc", access, "k3", "198.51.100.1",
		map[string]any{"national_id": "12345678", "full_name": "Jane Wanjiru Kamau", "date_of_birth": "1990-05-17"}); c != 202 || out["status"] != "pending" {
		t.Fatalf("kyc submit = %d %v", c, out)
	}
	deadline := time.Now().Add(40 * time.Second) //nolint:forbidigo // test polling deadline
	for {
		_, out := call(t, srv, http.MethodGet, "/v1/kyc", access, "x", "", nil)
		if out["status"] == "verified" {
			break
		}
		if time.Now().After(deadline) { //nolint:forbidigo // test polling deadline
			t.Fatalf("worker never verified the submission, status %v", out["status"])
		}
		time.Sleep(200 * time.Millisecond) //nolint:forbidigo // polling helper
	}
}

func TestApp_PerClientLimitsUseTheTrustedProxyEntryNotWhatTheClientClaims(t *testing.T) {
	ctx := context.Background()
	g := newGateways(t)
	vk := testvalkey.Start(t)
	pool := testpg.Pool(t, "app_worker")
	cfg := baseCfg(t, g, vk)
	cfg.WorkersInProcess = false
	a, err := app.New(ctx, cfg, pool)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close(ctx) })
	srv := httptest.NewServer(httpx.NewRouterWith(pool, a.Mount))
	t.Cleanup(srv.Close)

	// 61 login attempts for different numbers from one real client (the rightmost entry), each claiming a different
	// leftmost address: the per-IP limit (60/hour) must trip, because the spoofed entries are ignored.
	var last int
	for i := range 61 {
		last, _ = call(t, srv, http.MethodPost, "/v1/auth/login", "", fmt.Sprintf("key-%d", i),
			fmt.Sprintf("9.9.9.%d, 203.0.113.50", i%250),
			map[string]any{"msisdn": fmt.Sprintf("07%08d", i), "password": "whatever it is 1", "device": map[string]any{"fingerprint": "f", "platform": "android"}})
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("61st attempt = %d, want 429: a client must not dodge limits by inventing X-Forwarded-For entries", last)
	}
}

func TestApp_APIIsAbsentWhenNotConfigured(t *testing.T) {
	pool := testpg.Pool(t, "app_worker")
	a, err := app.New(context.Background(), config.Config{DatabaseURL: "x"}, pool)
	if err != nil {
		t.Fatal(err)
	}
	if a.Mount != nil {
		t.Fatal("API mounted without configuration")
	}
	if err := a.StartWorkers(context.Background()); err != nil {
		t.Fatalf("StartWorkers on a health-only deployment should be a no-op: %v", err)
	}
	a.Close(context.Background())
}
