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
	"github.com/itsivali/lottery-platform/internal/ledger"
	"github.com/itsivali/lottery-platform/internal/payments"
	"github.com/itsivali/lottery-platform/internal/payments/daraja"
	"github.com/itsivali/lottery-platform/internal/payments/darajamock"
	"github.com/itsivali/lottery-platform/internal/platform/clock"
	"github.com/itsivali/lottery-platform/internal/platform/crypto"
	"github.com/itsivali/lottery-platform/internal/platform/httpx"
	"github.com/itsivali/lottery-platform/internal/platform/idempotency"
	"github.com/itsivali/lottery-platform/internal/platform/ratelimit"
	"github.com/itsivali/lottery-platform/internal/platform/testpg"
	"github.com/itsivali/lottery-platform/internal/platform/testvalkey"
	"github.com/itsivali/lottery-platform/internal/rg"
	"github.com/itsivali/lottery-platform/internal/wallet"
)

// payQueue stands in for River for payment jobs: the API tests only check what was enqueued.
type payQueue struct {
	mu   sync.Mutex
	jobs []string
}

func (q *payQueue) EnqueuePush(_ context.Context, _ pgx.Tx, id string) error {
	return q.add("push:" + id)
}
func (q *payQueue) EnqueueVerify(_ context.Context, _ pgx.Tx, id string, src payments.VerifySource) error {
	return q.add("verify:" + string(src) + ":" + id)
}

func (q *payQueue) EnqueueC2B(_ context.Context, _ pgx.Tx, id string, src payments.VerifySource) error {
	return q.add("c2b:" + string(src) + ":" + id)
}

func (q *payQueue) add(s string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.jobs = append(q.jobs, s)
	return nil
}

const callbackPath = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

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
	kyc  *kyc.Mock
	vk   *testvalkey.Server
	srv  *httptest.Server
	enq  *kycQueue
	pay  *payQueue
	keys int
}

func setup(t *testing.T) *env {
	t.Helper()
	pool := testpg.Pool(t, "app_api")
	vk := testvalkey.Start(t)
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	clk := clock.NewManual(time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC))
	aw := audit.NewWriter()
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	kr, _ := crypto.NewKeyring(1, map[byte][]byte{1: k})
	mock := kyc.NewMock()
	enq := &kycQueue{}
	pay := &payQueue{}
	dm := httptest.NewServer(darajamock.New(darajamock.Options{ConsumerKey: "ck", ConsumerSecret: "cs", ShortCode: "174379", Passkey: "pk"}))
	t.Cleanup(dm.Close)
	dc, err := daraja.New(daraja.Config{BaseURL: dm.URL, ConsumerKey: "ck", ConsumerSecret: "cs", ShortCode: "174379", Passkey: "pk",
		CallbackURL: "http://api/hooks/mpesa/stk/" + callbackPath}, dm.Client(), clk)
	if err != nil {
		t.Fatal(err)
	}
	if dc, err = dc.WithC2B(daraja.C2BConfig{ShortCode: "600982", Initiator: "i", SecurityCredential: "c",
		ResultURL: "http://api/hooks/c2b/" + callbackPath + "/status-result", TimeoutURL: "http://api/hooks/c2b/" + callbackPath + "/status-timeout"}); err != nil {
		t.Fatal(err)
	}
	ids := identity.NewService(pool, clk, fakeOIDC{}, aw, identity.Config{
		AuthKey: []byte("0123456789abcdef0123456789abcdef"), TokenKey: priv, Issuer: "lottery", Audience: "lottery-api",
		AccessTTL: 10 * time.Minute, RefreshTTL: 30 * 24 * time.Hour, DeviceCap: 2,
	})
	rgs := rg.NewService(pool, clk, aw, rg.Config{CoolingOff: 24 * time.Hour})
	ls := ledger.NewStore()
	deps := api.Deps{
		Identity:     ids,
		KYC:          kyc.NewService(pool, clk, mock, kr, []byte("0123456789abcdef0123456789abcdef"), aw, enq),
		RG:           rgs,
		Payments:     payments.NewService(pool, clk, dc, pay, ids, rgs, wallet.NewService(ls), ls, aw, payments.Config{}).WithC2B(dc, payments.C2BConfig{ShortCode: "600982"}),
		CallbackPath: callbackPath,
		Idem:         idempotency.NewStore(pool),
		Limiter:      ratelimit.New(vk.Client(t)),
	}
	srv := httptest.NewServer(httpx.NewRouterWith(pool, api.Mount(deps)))
	enq.process = func(ctx context.Context, id string) error { return deps.KYC.Process(ctx, id) }
	t.Cleanup(srv.Close)
	return &env{pool: pool, clk: clk, kyc: mock, vk: vk, srv: srv, enq: enq, pay: pay}
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

const strongPassword = "correct horse battery"

func (e *env) signIn(t *testing.T) session {
	t.Helper()
	m := e.msisdn()
	fp := fmt.Sprintf("fp-%d", seq)
	r := e.do(t, http.MethodPost, "/v1/auth/signup", "", map[string]any{
		"msisdn": m, "password": strongPassword, "device": map[string]any{"fingerprint": fp, "platform": "android"},
	})
	if r.Code != http.StatusCreated {
		t.Fatalf("signup: %d %s", r.Code, r.Raw)
	}
	norm, _ := identity.NormalizeMSISDN(m)
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
	{http.MethodPost, "/v1/auth/sso/google/link"}, {http.MethodPost, "/v1/auth/password/change"},
	{http.MethodGet, "/v1/kyc"}, {http.MethodPost, "/v1/kyc"},
	{http.MethodGet, "/v1/rg/limits"}, {http.MethodPut, "/v1/rg/limits"},
	{http.MethodPost, "/v1/rg/timeout"}, {http.MethodPost, "/v1/rg/self-exclusion"},
	{http.MethodPost, "/v1/payments/topups"}, {http.MethodGet, "/v1/payments/0192f000-0000-7000-8000-000000000000"},
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
		{http.MethodPost, "/v1/auth/signup"}, {http.MethodPost, "/v1/auth/login"},
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

func TestAPI_LoginGuessingIsStoppedPerNumberAndFailsClosedWithoutValkey(t *testing.T) {
	e := setup(t)
	s := e.signIn(t)
	login := func(pw string) resp {
		return e.do(t, http.MethodPost, "/v1/auth/login", "", map[string]any{
			"msisdn": s.msisdn, "password": pw, "device": map[string]any{"fingerprint": s.fp, "platform": "android"},
		})
	}
	for i := 1; i <= 10; i++ {
		if r := login("not the password " + fmt.Sprint(i)); r.Code != http.StatusUnauthorized {
			t.Fatalf("guess %d = %d, want 401", i, r.Code)
		}
	}
	r := login(strongPassword) // even the right password is refused once the budget for this number is spent
	if r.Code != http.StatusTooManyRequests || r.Header.Get("Retry-After") == "" {
		t.Fatalf("11th attempt: %d retry-after=%q, want 429", r.Code, r.Header.Get("Retry-After"))
	}
	e.vk.Stop()
	if r := e.do(t, http.MethodPost, "/v1/auth/signup", "", map[string]any{"msisdn": e.msisdn(), "password": strongPassword, "device": map[string]any{"fingerprint": "x", "platform": "android"}}); r.Code != http.StatusServiceUnavailable {
		t.Fatalf("Valkey down: %d, want 503 (fail closed)", r.Code)
	}
}

func TestAPI_SignupIsRateLimitedPerNumber(t *testing.T) {
	e := setup(t)
	m := e.msisdn()
	var last int
	for i := range 6 {
		last = e.do(t, http.MethodPost, "/v1/auth/signup", "", map[string]any{
			"msisdn": m, "password": fmt.Sprintf("claim password %d", i), "device": map[string]any{"fingerprint": fmt.Sprintf("claimer-%d", i), "platform": "android"},
		}).Code
		if i < 5 && last != http.StatusCreated {
			t.Fatalf("claim %d = %d, want 201", i, last)
		}
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("6th claim of one number = %d, want 429", last)
	}
}

func TestAPI_LoginAndPasswordChange(t *testing.T) {
	e := setup(t)
	s := e.signIn(t)
	dev := map[string]any{"fingerprint": s.fp, "platform": "android"}
	r := e.do(t, http.MethodPost, "/v1/auth/login", "", map[string]any{"msisdn": s.msisdn, "password": strongPassword, "device": dev})
	if r.Code != http.StatusOK || r.Body["access_token"] == nil {
		t.Fatalf("login: %d %s", r.Code, r.Raw)
	}
	other := r.Body["access_token"].(string)
	for name, body := range map[string]map[string]any{
		"wrong current": {"current_password": "nope nope nope", "new_password": "a brand new passphrase"},
		"weak new":      {"current_password": strongPassword, "new_password": "password"},
		"unknown field": {"current_password": strongPassword, "new_password": "a brand new passphrase", "user_id": "x"},
	} {
		want := map[string]int{"wrong current": 403, "weak new": 400, "unknown field": 400}[name]
		if got := e.do(t, http.MethodPost, "/v1/auth/password/change", s.access, body); got.Code != want {
			t.Errorf("%s: %d, want %d (%s)", name, got.Code, want, got.Raw)
		}
	}
	if c := e.do(t, http.MethodPost, "/v1/auth/password/change", s.access, map[string]any{"current_password": strongPassword, "new_password": "a brand new passphrase"}); c.Code != 204 {
		t.Fatalf("change: %d %s", c.Code, c.Raw)
	}
	if got := e.do(t, http.MethodGet, "/v1/kyc", other, nil); got.Code != http.StatusUnauthorized {
		t.Errorf("another session survived the password change: %d", got.Code)
	}
	if got := e.do(t, http.MethodGet, "/v1/kyc", s.access, nil); got.Code != http.StatusOK {
		t.Errorf("the session that changed it was ended: %d", got.Code)
	}
	if got := e.do(t, http.MethodPost, "/v1/auth/login", "", map[string]any{"msisdn": s.msisdn, "password": strongPassword, "device": dev}); got.Code != http.StatusUnauthorized {
		t.Errorf("old password still works: %d", got.Code)
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
		{"invalid number", http.MethodPost, "/v1/auth/signup", map[string]any{"msisdn": "12345", "password": strongPassword, "device": map[string]any{"fingerprint": "f", "platform": "android"}}, 400},
		{"weak password", http.MethodPost, "/v1/auth/signup", map[string]any{"msisdn": "0712345678", "password": "password", "device": map[string]any{"fingerprint": "f", "platform": "android"}}, 400},
		{"missing device fingerprint", http.MethodPost, "/v1/auth/signup", map[string]any{"msisdn": "0712345678", "password": strongPassword, "device": map[string]any{"platform": "android"}}, 400},
		{"unknown field", http.MethodPost, "/v1/auth/signup", map[string]any{"msisdn": "0712345678", "password": strongPassword, "admin": true}, 400},
		{"not json", http.MethodPost, "/v1/auth/signup", "hello", 400},
		{"oversized body", http.MethodPost, "/v1/auth/signup", `{"msisdn":"` + strings.Repeat("1", 2<<20) + `"}`, 413},
		{"unknown account", http.MethodPost, "/v1/auth/login", map[string]any{"msisdn": "0712345678", "password": strongPassword, "device": map[string]any{"fingerprint": "f", "platform": "android"}}, 401},
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
	// the attestation verdict is not part of the request contract; sending one is refused
	r := e.do(t, http.MethodPost, "/v1/auth/signup", "", `{"msisdn":"`+m+`","password":"`+strongPassword+`","device":{"fingerprint":"fp","platform":"android","attestation":"passed"}}`)
	if r.Code != http.StatusBadRequest {
		t.Fatalf("client attestation claim accepted: %d %s", r.Code, r.Raw)
	}
	// an opaque token is accepted but, with no verifier configured, never yields a pass
	r = e.do(t, http.MethodPost, "/v1/auth/signup", "", map[string]any{"msisdn": m, "password": strongPassword, "device": map[string]any{"fingerprint": "fp", "platform": "android", "attestation_token": "looks-valid"}})
	if r.Code != http.StatusCreated {
		t.Fatalf("signup: %d %s", r.Code, r.Raw)
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

// verified marks the session's account KYC-verified, as the KYC worker would after a registry match.
func (e *env) verified(t *testing.T, s session) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(), `UPDATE identity.users SET kyc_status = 'verified', national_id_hash = $1 WHERE msisdn = $2`,
		[]byte(s.msisdn), s.msisdn); err != nil {
		t.Fatal(err)
	}
}

func TestAPI_TopUpStartsAPushAndOnlyItsOwnerCanSeeIt(t *testing.T) {
	e := setup(t)
	a, b := e.signIn(t), e.signIn(t)
	e.verified(t, a)
	r := e.do(t, http.MethodPost, "/v1/payments/topups", a.access, map[string]any{"amount_kes": 150})
	if r.Code != http.StatusAccepted || r.Body["status"] != "pending" || r.Body["amount_requested_kes"] != float64(150) {
		t.Fatalf("top-up: %d %s", r.Code, r.Raw)
	}
	id, _ := r.Body["id"].(string)
	if len(e.pay.jobs) != 1 || e.pay.jobs[0] != "push:"+id {
		t.Fatalf("jobs = %v", e.pay.jobs)
	}
	if r := e.do(t, http.MethodGet, "/v1/payments/"+id, a.access, nil); r.Code != http.StatusOK || r.Body["id"] != id || r.Body["amount_confirmed_kes"] != nil {
		t.Fatalf("owner get: %d %s", r.Code, r.Raw)
	}
	if r := e.do(t, http.MethodGet, "/v1/payments/"+id, b.access, nil); r.Code != http.StatusNotFound {
		t.Fatalf("other player's payment: %d %s", r.Code, r.Raw)
	}
	if r := e.do(t, http.MethodGet, "/v1/payments/nonsense", a.access, nil); r.Code != http.StatusNotFound {
		t.Fatalf("bad id: %d", r.Code)
	}
}

func TestAPI_TopUpErrorMapping(t *testing.T) {
	e := setup(t)
	s := e.signIn(t)
	if r := e.do(t, http.MethodPost, "/v1/payments/topups", s.access, map[string]any{"amount_kes": 100}); r.Code != http.StatusForbidden || !strings.HasSuffix(r.Body["type"].(string), "kyc-required") {
		t.Fatalf("unverified: %d %s", r.Code, r.Raw)
	}
	e.verified(t, s)
	for _, body := range []any{map[string]any{"amount_kes": 1}, map[string]any{"amount_kes": 250001}, `{"amount_kes":"100"}`, `{"amount_kes":100,"msisdn":"254700000000"}`} {
		if r := e.do(t, http.MethodPost, "/v1/payments/topups", s.access, body); r.Code != http.StatusBadRequest {
			t.Errorf("%v: %d %s", body, r.Code, r.Raw)
		}
	}
	if r := e.do(t, http.MethodPut, "/v1/rg/limits", s.access, map[string]any{"period": "daily", "amount_kes": 100}); r.Code != http.StatusOK {
		t.Fatalf("set limit: %d", r.Code)
	}
	r := e.do(t, http.MethodPost, "/v1/payments/topups", s.access, map[string]any{"amount_kes": 101})
	if r.Code != http.StatusForbidden || !strings.HasSuffix(r.Body["type"].(string), "deposit-limit") {
		t.Fatalf("over limit: %d %s", r.Code, r.Raw)
	}
	if r := e.do(t, http.MethodPost, "/v1/rg/timeout", s.access, map[string]any{"duration": "24h"}); r.Code != http.StatusNoContent {
		t.Fatalf("timeout: %d", r.Code)
	}
	r = e.do(t, http.MethodPost, "/v1/payments/topups", s.access, map[string]any{"amount_kes": 50})
	if r.Code != http.StatusForbidden || !strings.HasSuffix(r.Body["type"].(string), "restricted") {
		t.Fatalf("timed out: %d %s", r.Code, r.Raw)
	}
}

func (e *env) callback(t *testing.T, path, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, e.srv.URL+"/hooks/mpesa/stk/"+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Body.Close() }()
	raw, _ := io.ReadAll(r.Body)
	return r.StatusCode, string(raw)
}

func TestAPI_STKCallbackNeedsTheSecretPathAndIsAcknowledgedWithoutAnIdempotencyKey(t *testing.T) {
	e := setup(t)
	body := `{"Body":{"stkCallback":{"CheckoutRequestID":"ws_CO_x","ResultCode":1032,"ResultDesc":"cancelled"}}}`
	if code, _ := e.callback(t, "wrong", body); code != http.StatusNotFound {
		t.Fatalf("wrong path: %d", code)
	}
	if code, _ := e.callback(t, callbackPath[:63]+"0", body); code != http.StatusNotFound {
		t.Fatalf("near-miss path: %d", code)
	}
	code, raw := e.callback(t, callbackPath, body)
	if code != http.StatusOK || !strings.Contains(raw, `"ResultCode":0`) {
		t.Fatalf("callback: %d %s", code, raw)
	}
	if code, _ := e.callback(t, callbackPath, `<xml/>`); code != http.StatusBadRequest {
		t.Fatalf("garbage: %d", code)
	}
}

func (e *env) hook(t *testing.T, url, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, e.srv.URL+url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Body.Close() }()
	raw, _ := io.ReadAll(r.Body)
	return r.StatusCode, string(raw)
}

func TestAPI_PaybillHooksNeedTheSecretPathAndAnswerDaraja(t *testing.T) {
	e := setup(t)
	confirmation := `{"TransactionType":"Pay Bill","TransID":"TJK1AB2CD3","TransTime":"20261024093112","TransAmount":"150.00",
"BusinessShortCode":"600982","BillRefNumber":"0700000000","MSISDN":"254708374149","FirstName":"John"}`
	for _, action := range []string{"validate", "confirm", "status-result", "status-timeout"} {
		if code, _ := e.hook(t, "/hooks/c2b/"+callbackPath[:63]+"0/"+action, confirmation); code != http.StatusNotFound {
			t.Errorf("%s with a wrong secret: %d", action, code)
		}
	}
	base := "/hooks/c2b/" + callbackPath
	if code, raw := e.hook(t, base+"/validate", confirmation); code != http.StatusOK || !strings.Contains(raw, `"ResultCode":"C2B00012"`) ||
		!strings.Contains(raw, `"Rejected"`) {
		t.Fatalf("validate an unknown account: %d %s", code, raw)
	}
	if code, raw := e.hook(t, base+"/confirm", confirmation); code != http.StatusOK || !strings.Contains(raw, `"ResultCode":0`) {
		t.Fatalf("confirm: %d %s", code, raw)
	}
	e.pay.mu.Lock()
	jobs := strings.Join(e.pay.jobs, ",")
	e.pay.mu.Unlock()
	if !strings.Contains(jobs, "c2b:callback:") {
		t.Fatalf("jobs = %s: the confirmation must be verified in the background", jobs)
	}
	if code, _ := e.hook(t, base+"/confirm", `<xml/>`); code != http.StatusBadRequest {
		t.Fatalf("garbage confirmation: %d", code)
	}
	if code, _ := e.hook(t, base+"/status-result", `{}`); code != http.StatusBadRequest {
		t.Fatalf("garbage status result: %d", code)
	}
	if code, raw := e.hook(t, base+"/status-result", `{"Result":{"ResultCode":1,"OriginatorConversationID":"nobody"}}`); code != http.StatusOK ||
		!strings.Contains(raw, `"ResultCode":0`) {
		t.Fatalf("status result: %d %s", code, raw)
	}
	if code, _ := e.hook(t, base+"/status-timeout", `not json`); code != http.StatusOK {
		t.Fatalf("status timeout: %d", code)
	}
}
