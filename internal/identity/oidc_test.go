package identity_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/itsivali/lottery-platform/internal/identity"
	"github.com/itsivali/lottery-platform/internal/platform/clock"
)

type oidcFixture struct {
	key     *rsa.PrivateKey
	kid     atomic.Value // current kid served
	fetches atomic.Int64
	srv     *httptest.Server
	clk     *clock.Manual
	v       *identity.OIDCVerifier
}

func newOIDC(t *testing.T) *oidcFixture {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &oidcFixture{key: key, clk: clock.NewManual(time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC))}
	f.kid.Store("k1")
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f.fetches.Add(1)
		e := big.NewInt(int64(key.E)).Bytes()
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "alg": "RS256", "use": "sig", "kid": f.kid.Load().(string),
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(e),
		}}})
	}))
	t.Cleanup(f.srv.Close)
	f.v = identity.NewOIDCVerifier(f.clk, http.DefaultClient, map[string]identity.OIDCProvider{
		"google": {Issuers: []string{"https://accounts.google.com", "accounts.google.com"}, Audiences: []string{"client-123"}, JWKSURL: f.srv.URL},
		"apple":  {Issuers: []string{"https://appleid.apple.com"}, Audiences: []string{"com.example.lottery"}, JWKSURL: f.srv.URL, NonceSHA256: true},
	})
	return f
}

func (f *oidcFixture) token(t *testing.T, kid string, method jwt.SigningMethod, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(method, claims)
	tok.Header["kid"] = kid
	var signKey any = f.key
	if method == jwt.SigningMethodHS256 {
		signKey = []byte("whatever")
	}
	s, err := tok.SignedString(signKey)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (f *oidcFixture) claims(overrides jwt.MapClaims) jwt.MapClaims {
	now, _ := f.clk.Now(context.Background(), nil)
	c := jwt.MapClaims{"iss": "https://accounts.google.com", "aud": "client-123", "sub": "g-42", "nonce": "n1",
		"iat": now.Unix(), "exp": now.Add(10 * time.Minute).Unix()}
	for k, v := range overrides {
		if v == nil {
			delete(c, k)
		} else {
			c[k] = v
		}
	}
	return c
}

func TestOIDC_AcceptsAValidGoogleToken(t *testing.T) {
	f := newOIDC(t)
	sub, err := f.v.VerifyIDToken(context.Background(), "google", f.token(t, "k1", jwt.SigningMethodRS256, f.claims(nil)), "n1")
	if err != nil || sub != "g-42" {
		t.Fatalf("sub = %q, %v", sub, err)
	}
}

func TestOIDC_AppleNonceIsHashed(t *testing.T) {
	f := newOIDC(t)
	h := sha256.Sum256([]byte("n1"))
	c := f.claims(jwt.MapClaims{"iss": "https://appleid.apple.com", "aud": "com.example.lottery", "nonce": hex.EncodeToString(h[:])})
	if sub, err := f.v.VerifyIDToken(context.Background(), "apple", f.token(t, "k1", jwt.SigningMethodRS256, c), "n1"); err != nil || sub != "g-42" {
		t.Fatalf("hashed nonce rejected: %q, %v", sub, err)
	}
	c["nonce"] = "n1" // raw nonce is not what Apple sends and must not pass for an apple provider
	if _, err := f.v.VerifyIDToken(context.Background(), "apple", f.token(t, "k1", jwt.SigningMethodRS256, c), "n1"); err == nil {
		t.Fatal("raw nonce accepted for apple")
	}
}

func TestOIDC_RejectsBadTokens(t *testing.T) {
	f := newOIDC(t)
	ctx := context.Background()
	cases := map[string]string{
		"wrong issuer":    f.token(t, "k1", jwt.SigningMethodRS256, f.claims(jwt.MapClaims{"iss": "https://evil.example"})),
		"wrong audience":  f.token(t, "k1", jwt.SigningMethodRS256, f.claims(jwt.MapClaims{"aud": "someone-else"})),
		"expired":         f.token(t, "k1", jwt.SigningMethodRS256, f.claims(jwt.MapClaims{"exp": f.now().Add(-time.Minute).Unix()})),
		"no expiry":       f.token(t, "k1", jwt.SigningMethodRS256, f.claims(jwt.MapClaims{"exp": nil})),
		"wrong nonce":     f.token(t, "k1", jwt.SigningMethodRS256, f.claims(jwt.MapClaims{"nonce": "other"})),
		"missing nonce":   f.token(t, "k1", jwt.SigningMethodRS256, f.claims(jwt.MapClaims{"nonce": nil})),
		"missing subject": f.token(t, "k1", jwt.SigningMethodRS256, f.claims(jwt.MapClaims{"sub": nil})),
		"hmac confusion":  f.token(t, "k1", jwt.SigningMethodHS256, f.claims(nil)),
		"unknown kid":     f.token(t, "nope", jwt.SigningMethodRS256, f.claims(nil)),
		"garbage":         "not.a.jwt",
		"empty":           "",
	}
	for name, tok := range cases {
		if _, err := f.v.VerifyIDToken(ctx, "google", tok, "n1"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := f.v.VerifyIDToken(ctx, "facebook", f.token(t, "k1", jwt.SigningMethodRS256, f.claims(nil)), "n1"); err == nil {
		t.Error("unknown provider accepted")
	}
	if _, err := f.v.VerifyIDToken(ctx, "google", f.token(t, "k1", jwt.SigningMethodRS256, f.claims(nil)), ""); err == nil {
		t.Error("empty expected nonce accepted")
	}
}

func TestOIDC_CachesKeysAndRefetchesOnRotation(t *testing.T) {
	f := newOIDC(t)
	ctx := context.Background()
	tok := f.token(t, "k1", jwt.SigningMethodRS256, f.claims(nil))
	for range 3 {
		if _, err := f.v.VerifyIDToken(ctx, "google", tok, "n1"); err != nil {
			t.Fatal(err)
		}
	}
	if f.fetches.Load() != 1 {
		t.Fatalf("%d JWKS fetches for 3 verifications, want 1 (cached)", f.fetches.Load())
	}
	// provider rotates its key id; after the minimum refetch interval an unknown kid triggers one refresh
	f.kid.Store("k2")
	rotated := f.token(t, "k2", jwt.SigningMethodRS256, f.claims(nil))
	if _, err := f.v.VerifyIDToken(ctx, "google", rotated, "n1"); err == nil {
		t.Fatal("rotation picked up before the refetch interval elapsed (hammering protection missing)")
	}
	f.clk.Advance(2 * time.Minute)
	if _, err := f.v.VerifyIDToken(ctx, "google", rotated, "n1"); err != nil {
		t.Fatalf("rotated key not picked up: %v", err)
	}
}

func (f *oidcFixture) now() time.Time { n, _ := f.clk.Now(context.Background(), nil); return n }

func TestOIDC_CacheBoundariesAreExact(t *testing.T) {
	ctx := context.Background()
	tokenFor := func(f *oidcFixture, kid string) string { return f.token(t, kid, jwt.SigningMethodRS256, f.claims(nil)) }

	// an unknown key id may trigger a refetch only once a full minute has passed since the last fetch
	f := newOIDC(t)
	if _, err := f.v.VerifyIDToken(ctx, "google", tokenFor(f, "k1"), "n1"); err != nil {
		t.Fatal(err)
	}
	f.kid.Store("k2")
	f.clk.Advance(59 * time.Second)
	if _, err := f.v.VerifyIDToken(ctx, "google", tokenFor(f, "k2"), "n1"); err == nil {
		t.Fatal("refetched after 59s: the throttle is too loose")
	}
	f.clk.Advance(time.Second) // exactly one minute since the fetch
	if _, err := f.v.VerifyIDToken(ctx, "google", tokenFor(f, "k2"), "n1"); err != nil {
		t.Fatalf("not refetched at exactly one minute: %v", err)
	}

	// cached keys expire at exactly six hours, even for a key id we know
	g := newOIDC(t)
	if _, err := g.v.VerifyIDToken(ctx, "google", tokenFor(g, "k1"), "n1"); err != nil {
		t.Fatal(err)
	}
	g.clk.Advance(6*time.Hour - time.Second)
	if _, err := g.v.VerifyIDToken(ctx, "google", g.token(t, "k1", jwt.SigningMethodRS256, g.claims(jwt.MapClaims{"exp": g.now().Add(time.Hour).Unix()})), "n1"); err != nil {
		t.Fatal(err)
	}
	if g.fetches.Load() != 1 {
		t.Fatalf("%d fetches before the cache expired, want 1", g.fetches.Load())
	}
	g.clk.Advance(time.Second) // exactly six hours
	if _, err := g.v.VerifyIDToken(ctx, "google", g.token(t, "k1", jwt.SigningMethodRS256, g.claims(jwt.MapClaims{"exp": g.now().Add(time.Hour).Unix()})), "n1"); err != nil {
		t.Fatal(err)
	}
	if g.fetches.Load() != 2 {
		t.Fatalf("%d fetches after exactly six hours, want 2 (cache must expire)", g.fetches.Load())
	}
}
