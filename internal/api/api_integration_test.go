//go:build integration

package api_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/itsivali/lottery-platform/internal/api"
	"github.com/itsivali/lottery-platform/internal/audit"
	"github.com/itsivali/lottery-platform/internal/identity"
	"github.com/itsivali/lottery-platform/internal/kyc"
	"github.com/itsivali/lottery-platform/internal/platform/clock"
	"github.com/itsivali/lottery-platform/internal/platform/crypto"
	"github.com/itsivali/lottery-platform/internal/platform/httpx"
	"github.com/itsivali/lottery-platform/internal/platform/idempotency"
	"github.com/itsivali/lottery-platform/internal/platform/ratelimit"
	"github.com/itsivali/lottery-platform/internal/platform/testpg"
	"github.com/itsivali/lottery-platform/internal/platform/testvalkey"
	"github.com/itsivali/lottery-platform/internal/rg"
)

type fakeSMS struct {
	mu    sync.Mutex
	codes map[string]string
}

var sixDigits = regexp.MustCompile(`\b\d{6}\b`)

func (f *fakeSMS) Send(_ context.Context, msisdn, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.codes == nil {
		f.codes = map[string]string{}
	}
	f.codes[msisdn] = sixDigits.FindString(text)
	return nil
}

func (f *fakeSMS) code(m string) string { f.mu.Lock(); defer f.mu.Unlock(); return f.codes[m] }

type fakeOIDC struct{}

func (fakeOIDC) VerifyIDToken(_ context.Context, _, token, nonce string) (string, error) {
	sub, ok := strings.CutPrefix(token, "ok:")
	if !ok || nonce != "n1" {
		return "", errors.New("bad token")
	}
	return sub, nil
}

// kycQueue stands in for River: it remembers enqueued checks so a test can run them when it chooses.
type kycQueue struct {
	mu      sync.Mutex
	pending []string
	process func(context.Context, string) error
}

func (q *kycQueue) EnqueueVerify(_ context.Context, _ pgx.Tx, checkID, _ string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.pending = append(q.pending, checkID)
	return nil
}

// drain runs every queued verification job, as the worker would.
func (e *env) drainKYC(t *testing.T) {
	t.Helper()
	e.enq.mu.Lock()
	jobs := e.enq.pending
	e.enq.pending = nil
	e.enq.mu.Unlock()
	for _, id := range jobs {
		if err := e.enq.process(context.Background(), id); err != nil {
			t.Fatalf("kyc job %s: %v", id, err)
		}
	}
}

type env struct {
	pool *pgxpool.Pool
	clk  *clock.Manual
	sms  *fakeSMS
	kyc  *kyc.Mock
	vk   *testvalkey.Server
	srv  *httptest.Server
	enq  *kycQueue
	keys int
}

func setup(t *testing.T) *env {
	t.Helper()
	pool := testpg.Pool(t, "app_api")
	vk := testvalkey.Start(t)
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	clk := clock.NewManual(time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC))
	sms := &fakeSMS{}
	aw := audit.NewWriter()
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	kr, _ := crypto.NewKeyring(1, map[byte][]byte{1: k})
	mock := kyc.NewMock()
	enq := &kycQueue{}
	deps := api.Deps{
		Identity: identity.NewService(pool, clk, sms, fakeOIDC{}, aw, identity.Config{
			OTPKey: []byte("0123456789abcdef0123456789abcdef"), TokenKey: priv, Issuer: "lottery", Audience: "lottery-api",
			AccessTTL: 10 * time.Minute, RefreshTTL: 30 * 24 * time.Hour, OTPTTL: 5 * time.Minute, OTPCooldown: time.Minute, DeviceCap: 2,
		}),
		KYC:     kyc.NewService(pool, clk, mock, kr, []byte("0123456789abcdef0123456789abcdef"), aw, enq),
		RG:      rg.NewService(pool, clk, aw, rg.Config{CoolingOff: 24 * time.Hour}),
		Idem:    idempotency.NewStore(pool),
		Limiter: ratelimit.New(vk.Client(t)),
	}
	srv := httptest.NewServer(httpx.NewRouterWith(pool, api.Mount(deps)))
	enq.process = func(ctx context.Context, id string) error { return deps.KYC.Process(ctx, id) }
	t.Cleanup(srv.Close)
	return &env{pool: pool, clk: clk, sms: sms, kyc: mock, vk: vk, srv: srv, enq: enq}
}

type resp struct {
	Code   int
	Header http.Header
	Body   map[string]any
	Raw    string
}

func (e *env) do(t *testing.T, method, path, token string, body any) resp {
	t.Helper()
	e.keys++
	return e.doKey(t, method, path, token, fmt.Sprintf("key-%d", e.keys), body)
}

func (e *env) doKey(t *testing.T, method, path, token, key string, body any) resp {
	t.Helper()
	var rd io.Reader = http.NoBody
	if s, ok := body.(string); ok {
		rd = strings.NewReader(s)
	} else if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequestWithContext(context.Background(), method, e.srv.URL+path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	req.Header.Set("Content-Type", "application/json")
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Body.Close() }()
	raw, _ := io.ReadAll(r.Body)
	out := resp{Code: r.StatusCode, Header: r.Header, Raw: string(raw)}
	_ = json.Unmarshal(raw, &out.Body)
	return out
}

var seq int

func (e *env) msisdn() string { seq++; return fmt.Sprintf("07%08d", seq) }

type session struct{ access, refresh, fp, msisdn string }

func (e *env) signIn(t *testing.T) session {
	t.Helper()
	m := e.msisdn()
	fp := fmt.Sprintf("fp-%d", seq)
	if r := e.do(t, http.MethodPost, "/v1/auth/otp/request", "", map[string]any{"msisdn": m}); r.Code != http.StatusNoContent {
		t.Fatalf("otp request: %d %s", r.Code, r.Raw)
	}
	norm, _ := identity.NormalizeMSISDN(m)
	r := e.do(t, http.MethodPost, "/v1/auth/otp/verify", "", map[string]any{
		"msisdn": m, "code": e.sms.code(norm), "device": map[string]any{"fingerprint": fp, "platform": "android"},
	})
	if r.Code != http.StatusOK {
		t.Fatalf("otp verify: %d %s", r.Code, r.Raw)
	}
	return session{access: r.Body["access_token"].(string), refresh: r.Body["refresh_token"].(string), fp: fp, msisdn: norm}
}

func TestAPI_SignInRefreshAndLogoutFlow(t *testing.T) {
	e := setup(t)
	s := e.signIn(t)
	if r := e.do(t, http.MethodGet, "/v1/kyc", s.access, nil); r.Code != http.StatusOK || r.Body["status"] != "none" {
		t.Fatalf("authenticated call: %d %s", r.Code, r.Raw)
	}
	r := e.do(t, http.MethodPost, "/v1/auth/refresh", "", map[string]any{"refresh_token": s.refresh, "device_fingerprint": s.fp})
	if r.Code != http.StatusOK || r.Body["refresh_token"] == s.refresh {
		t.Fatalf("refresh: %d %s", r.Code, r.Raw)
	}
	newAccess := r.Body["access_token"].(string)
	if r := e.do(t, http.MethodPost, "/v1/auth/logout", newAccess, nil); r.Code != http.StatusNoContent {
		t.Fatalf("logout: %d", r.Code)
	}
	if r := e.do(t, http.MethodGet, "/v1/kyc", newAccess, nil); r.Code != http.StatusUnauthorized {
		t.Fatalf("token still valid after logout: %d", r.Code)
	}
	// replaying the first refresh token is a reuse and must be refused
	if r := e.do(t, http.MethodPost, "/v1/auth/refresh", "", map[string]any{"refresh_token": s.refresh, "device_fingerprint": s.fp}); r.Code != http.StatusUnauthorized {
		t.Fatalf("reused refresh token: %d", r.Code)
	}
}

type route struct{ method, path string }

var authedRoutes = []route{
	{http.MethodPost, "/v1/auth/sso/google/link"},
	{http.MethodGet, "/v1/kyc"}, {http.MethodPost, "/v1/kyc"},
	{http.MethodGet, "/v1/rg/limits"}, {http.MethodPut, "/v1/rg/limits"},
	{http.MethodPost, "/v1/rg/timeout"}, {http.MethodPost, "/v1/rg/self-exclusion"},
	{http.MethodPost, "/v1/auth/logout"}, // last: it ends the session used for the valid-token check
}

func TestAPI_EveryAuthenticatedRouteRefusesMissingBadAndRevokedTokens(t *testing.T) {
	e := setup(t)
	good := e.signIn(t)
	loggedOut := e.signIn(t)
	e.do(t, http.MethodPost, "/v1/auth/logout", loggedOut.access, nil)
	other := setup(t).signIn(t) // signed by a different server key

	for _, rt := range authedRoutes {
		for name, tok := range map[string]string{
			"no token": "", "garbage": "abc.def.ghi", "logged-out session": loggedOut.access, "foreign signature": other.access,
		} {
			if r := e.do(t, rt.method, rt.path, tok, `{}`); r.Code != http.StatusUnauthorized {
				t.Errorf("%s %s with %s = %d, want 401", rt.method, rt.path, name, r.Code)
			}
		}
		if r := e.do(t, rt.method, rt.path, good.access, `{}`); r.Code == http.StatusUnauthorized {
			t.Errorf("%s %s rejected a valid token", rt.method, rt.path)
		}
	}
	// the token must be a Bearer credential
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, e.srv.URL+"/v1/kyc", http.NoBody)
	req.Header.Set("Authorization", good.access)
	req.Header.Set("Idempotency-Key", "x")
	r, _ := http.DefaultClient.Do(req)
	_ = r.Body.Close()
	if r.StatusCode != http.StatusUnauthorized {
		t.Errorf("non-Bearer scheme accepted: %d", r.StatusCode)
	}
}

func TestAPI_EveryWriteRequiresAnIdempotencyKey(t *testing.T) {
	e := setup(t)
	s := e.signIn(t)
	writes := append([]route{
		{http.MethodPost, "/v1/auth/otp/request"}, {http.MethodPost, "/v1/auth/otp/verify"},
		{http.MethodPost, "/v1/auth/refresh"}, {http.MethodPost, "/v1/auth/sso/google"},
	}, authedRoutes...)
	for _, rt := range writes {
		if rt.method == http.MethodGet {
			continue
		}
		if r := e.doKey(t, rt.method, rt.path, s.access, "", `{}`); r.Code != http.StatusBadRequest {
			t.Errorf("%s %s without key = %d, want 400", rt.method, rt.path, r.Code)
		}
	}
}

func TestAPI_PlayersCannotSeeOrChangeEachOthersData(t *testing.T) {
	e := setup(t)
	e.kyc.Register("12345678", "Jane Wanjiru Kamau", time.Date(1990, 5, 17, 0, 0, 0, 0, time.UTC))
	a, b := e.signIn(t), e.signIn(t)

	if r := e.do(t, http.MethodPost, "/v1/kyc", a.access, map[string]any{"national_id": "12345678", "full_name": "Jane Wanjiru Kamau", "date_of_birth": "1990-05-17"}); r.Code != http.StatusAccepted || r.Body["status"] != "pending" {
		t.Fatalf("A KYC submit: %d %s", r.Code, r.Raw)
	}
	e.drainKYC(t)
	if r := e.do(t, http.MethodGet, "/v1/kyc", a.access, nil); r.Body["status"] != "verified" {
		t.Fatalf("A KYC after the job ran: %s", r.Raw)
	}
	e.do(t, http.MethodPut, "/v1/rg/limits", a.access, map[string]any{"period": "daily", "amount_kes": 500})
	e.do(t, http.MethodPost, "/v1/rg/self-exclusion", a.access, map[string]any{"term": "permanent"})

	if r := e.do(t, http.MethodGet, "/v1/kyc", b.access, nil); r.Body["status"] != "none" {
		t.Errorf("B sees A's KYC status: %s", r.Raw)
	}
	if r := e.do(t, http.MethodGet, "/v1/rg/limits", b.access, nil); strings.Contains(r.Raw, "500") {
		t.Errorf("B sees A's limits: %s", r.Raw)
	}
	// B cannot name A as the target of an action: any user_id in a body is an unknown field
	for _, rt := range []struct {
		path string
		body map[string]any
	}{
		{"/v1/kyc", map[string]any{"national_id": "99999999", "full_name": "Bob Builder", "date_of_birth": "1990-01-01", "user_id": "someone"}},
		{"/v1/rg/limits", map[string]any{"period": "daily", "amount_kes": 1, "user_id": "someone"}},
		{"/v1/rg/timeout", map[string]any{"duration": "24h", "user_id": "someone"}},
		{"/v1/rg/self-exclusion", map[string]any{"term": "6m", "user_id": "someone"}},
	} {
		method := http.MethodPost
		if rt.path == "/v1/rg/limits" {
			method = http.MethodPut
		}
		if r := e.do(t, method, rt.path, b.access, rt.body); r.Code != http.StatusBadRequest {
			t.Errorf("%s with user_id = %d, want 400", rt.path, r.Code)
		}
	}
	var excluded int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM rg.restrictions`).Scan(&excluded)
	if excluded != 1 {
		t.Errorf("%d restrictions exist, want only A's", excluded)
	}
	// a path-embedded id must not exist either
	if r := e.do(t, http.MethodGet, "/v1/kyc/"+"00000000-0000-7000-8000-000000000001", b.access, nil); r.Code != http.StatusNotFound {
		t.Errorf("per-id KYC route exists: %d", r.Code)
	}
}

func TestAPI_ReplayedRequestsReturnTheStoredResponseWithoutRepeatingTheEffect(t *testing.T) {
	e := setup(t)
	s := e.signIn(t)
	body := map[string]any{"duration": "24h"}
	r1 := e.doKey(t, http.MethodPost, "/v1/rg/timeout", s.access, "same-key", body)
	r2 := e.doKey(t, http.MethodPost, "/v1/rg/timeout", s.access, "same-key", body)
	if r1.Code != http.StatusNoContent || r2.Code != http.StatusNoContent || r2.Header.Get("Idempotent-Replay") != "true" {
		t.Fatalf("codes %d/%d replay=%q", r1.Code, r2.Code, r2.Header.Get("Idempotent-Replay"))
	}
	var n int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM rg.restrictions`).Scan(&n)
	if n != 1 {
		t.Fatalf("%d restrictions after a replay, want 1", n)
	}
	if r := e.doKey(t, http.MethodPost, "/v1/rg/timeout", s.access, "same-key", map[string]any{"duration": "7d"}); r.Code != http.StatusUnprocessableEntity {
		t.Fatalf("same key with a different body = %d, want 422", r.Code)
	}
}

func TestAPI_OTPRequestsAreRateLimitedPerNumberAndFailClosedWithoutValkey(t *testing.T) {
	e := setup(t)
	m := e.msisdn()
	norm, _ := identity.NormalizeMSISDN(m)
	_ = norm
	for i := 1; i <= 5; i++ {
		r := e.do(t, http.MethodPost, "/v1/auth/otp/request", "", map[string]any{"msisdn": m})
		if i == 1 && r.Code != http.StatusNoContent {
			t.Fatalf("first request: %d", r.Code)
		}
		e.clk.Advance(2 * time.Minute) // clear the 60s resend cooldown so only the hourly limiter can refuse
	}
	r := e.do(t, http.MethodPost, "/v1/auth/otp/request", "", map[string]any{"msisdn": m})
	if r.Code != http.StatusTooManyRequests || r.Header.Get("Retry-After") == "" {
		t.Fatalf("6th request: %d retry-after=%q", r.Code, r.Header.Get("Retry-After"))
	}
	e.vk.Stop()
	if r := e.do(t, http.MethodPost, "/v1/auth/otp/request", "", map[string]any{"msisdn": e.msisdn()}); r.Code != http.StatusServiceUnavailable {
		t.Fatalf("Valkey down: %d, want 503 (fail closed)", r.Code)
	}
}

func TestAPI_ErrorMapping(t *testing.T) {
	e := setup(t)
	cases := []struct {
		name   string
		method string
		path   string
		body   any
		want   int
	}{
		{"invalid number", http.MethodPost, "/v1/auth/otp/request", map[string]any{"msisdn": "12345"}, 400},
		{"unknown field", http.MethodPost, "/v1/auth/otp/request", map[string]any{"msisdn": "0712345678", "admin": true}, 400},
		{"not json", http.MethodPost, "/v1/auth/otp/request", "hello", 400},
		{"oversized body", http.MethodPost, "/v1/auth/otp/request", `{"msisdn":"` + strings.Repeat("1", 2<<20) + `"}`, 413},
		{"wrong code", http.MethodPost, "/v1/auth/otp/verify", map[string]any{"msisdn": "0712345678", "code": "000000", "device": map[string]any{"fingerprint": "f", "platform": "android"}}, 401},
		{"unknown sso provider", http.MethodPost, "/v1/auth/sso/facebook", map[string]any{"id_token": "x", "nonce": "n1"}, 404},
		{"unlinked sso", http.MethodPost, "/v1/auth/sso/google", map[string]any{"id_token": "ok:nobody", "nonce": "n1"}, 409},
		{"bad refresh token", http.MethodPost, "/v1/auth/refresh", map[string]any{"refresh_token": "nope", "device_fingerprint": "f"}, 401},
	}
	for _, c := range cases {
		r := e.do(t, c.method, c.path, "", c.body)
		if r.Code != c.want {
			t.Errorf("%s: %d, want %d (%s)", c.name, r.Code, c.want, r.Raw)
		}
		if r.Code >= 400 && !strings.HasPrefix(r.Header.Get("Content-Type"), "application/problem+json") {
			t.Errorf("%s: error is not problem+json: %q", c.name, r.Header.Get("Content-Type"))
		}
	}
	// cooldown between OTP requests
	m := e.msisdn()
	e.do(t, http.MethodPost, "/v1/auth/otp/request", "", map[string]any{"msisdn": m})
	if r := e.do(t, http.MethodPost, "/v1/auth/otp/request", "", map[string]any{"msisdn": m}); r.Code != http.StatusTooManyRequests || r.Header.Get("Retry-After") == "" {
		t.Errorf("cooldown: %d retry-after=%q", r.Code, r.Header.Get("Retry-After"))
	}
}

func TestAPI_KYCErrorMapping(t *testing.T) {
	e := setup(t)
	e.kyc.Register("12345678", "Jane Wanjiru Kamau", time.Date(1990, 5, 17, 0, 0, 0, 0, time.UTC))
	a, b := e.signIn(t), e.signIn(t)
	submit := func(tok string, id, name, dob string) resp {
		return e.do(t, http.MethodPost, "/v1/kyc", tok, map[string]any{"national_id": id, "full_name": name, "date_of_birth": dob})
	}
	if r := submit(a.access, "abc", "Jane Doe", "1990-01-01"); r.Code != 400 {
		t.Errorf("invalid input: %d", r.Code)
	}
	if r := submit(a.access, "22222222", "Young Person", "2015-01-01"); r.Code != 403 {
		t.Errorf("underage: %d", r.Code)
	}
	if r := submit(a.access, "12345678", "Jane Wanjiru Kamau", "1990-05-17"); r.Code != 202 || r.Body["status"] != "pending" {
		t.Errorf("accepted for verification: %d %s", r.Code, r.Raw)
	}
	if r := submit(a.access, "12345678", "Jane Wanjiru Kamau", "1990-05-17"); r.Code != 409 {
		t.Errorf("resubmit while pending: %d", r.Code)
	}
	e.drainKYC(t)
	if r := e.do(t, http.MethodGet, "/v1/kyc", a.access, nil); r.Body["status"] != "verified" {
		t.Errorf("after the job: %s", r.Raw)
	}
	if r := submit(a.access, "12345678", "Jane Wanjiru Kamau", "1990-05-17"); r.Code != 409 {
		t.Errorf("already verified: %d", r.Code)
	}
	if r := submit(b.access, "12345678", "Jane Wanjiru Kamau", "1990-05-17"); r.Code != 409 {
		t.Errorf("duplicate id: %d", r.Code)
	}
	// a registry outage never fails the request: the submission stays pending and the queue retries
	e.kyc.FailNext(errors.New("down"))
	if r := submit(b.access, "33333333", "Some Body", "1990-01-01"); r.Code != 202 {
		t.Errorf("submission during an outage: %d", r.Code)
	}
	e.enq.mu.Lock()
	pending := e.enq.pending[0]
	e.enq.mu.Unlock()
	if err := e.enq.process(context.Background(), pending); err == nil {
		t.Error("the job should report the outage so it is retried")
	}
	if r := e.do(t, http.MethodGet, "/v1/kyc", b.access, nil); r.Body["status"] != "pending" {
		t.Errorf("status during the outage: %s", r.Raw)
	}
}

func TestAPI_ResponsibleGamingRoundTrip(t *testing.T) {
	e := setup(t)
	s := e.signIn(t)
	if r := e.do(t, http.MethodPut, "/v1/rg/limits", s.access, map[string]any{"period": "weekly", "amount_kes": 2000}); r.Code != 200 {
		t.Fatalf("set limit: %d %s", r.Code, r.Raw)
	}
	e.clk.Advance(time.Minute)
	e.do(t, http.MethodPut, "/v1/rg/limits", s.access, map[string]any{"period": "weekly", "amount_kes": 5000})
	r := e.do(t, http.MethodGet, "/v1/rg/limits", s.access, nil)
	weekly := r.Body["limits"].(map[string]any)["weekly"].(map[string]any)
	if weekly["current"].(map[string]any)["amount_kes"] != float64(2000) || weekly["pending"].(map[string]any)["amount_kes"] != float64(5000) {
		t.Fatalf("limits: %s", r.Raw)
	}
	for _, bad := range []struct {
		path string
		body map[string]any
	}{
		{"/v1/rg/timeout", map[string]any{"duration": "1h"}},
		{"/v1/rg/self-exclusion", map[string]any{"term": "2w"}},
	} {
		if r := e.do(t, http.MethodPost, bad.path, s.access, bad.body); r.Code != 400 {
			t.Errorf("%s invalid: %d", bad.path, r.Code)
		}
	}
	if r := e.do(t, http.MethodPut, "/v1/rg/limits", s.access, map[string]any{"period": "hourly", "amount_kes": 5}); r.Code != 400 {
		t.Errorf("invalid period: %d", r.Code)
	}
	if r := e.do(t, http.MethodPost, "/v1/rg/self-exclusion", s.access, map[string]any{"term": "6m"}); r.Code != 204 {
		t.Errorf("self-exclusion: %d", r.Code)
	}
}

func TestAPI_ClientClaimedAttestationIsNeverTrusted(t *testing.T) {
	e := setup(t)
	m := e.msisdn()
	norm, _ := identity.NormalizeMSISDN(m)
	e.do(t, http.MethodPost, "/v1/auth/otp/request", "", map[string]any{"msisdn": m})
	// the attestation field is not part of the request contract; sending it is refused
	r := e.do(t, http.MethodPost, "/v1/auth/otp/verify", "", `{"msisdn":"`+m+`","code":"`+e.sms.code(norm)+`","device":{"fingerprint":"fp","platform":"android","attestation":"passed"}}`)
	if r.Code != http.StatusBadRequest {
		t.Fatalf("client attestation claim accepted: %d %s", r.Code, r.Raw)
	}
	r = e.do(t, http.MethodPost, "/v1/auth/otp/verify", "", map[string]any{"msisdn": m, "code": e.sms.code(norm), "device": map[string]any{"fingerprint": "fp", "platform": "android"}})
	if r.Code != 200 {
		t.Fatalf("verify: %d %s", r.Code, r.Raw)
	}
	var verdict string
	_ = e.pool.QueryRow(context.Background(), `SELECT attestation_verdict FROM identity.devices`).Scan(&verdict)
	if verdict != "none" {
		t.Fatalf("verdict = %q, want none until server-side attestation exists", verdict)
	}
}

func TestAPI_HealthEndpointsStillWork(t *testing.T) {
	e := setup(t)
	for _, p := range []string{"/healthz", "/readyz"} {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, e.srv.URL+p, http.NoBody)
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = r.Body.Close()
		if r.StatusCode != 200 {
			t.Errorf("%s = %d", p, r.StatusCode)
		}
	}
}
