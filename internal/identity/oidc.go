package identity

import (
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/itsivali/lottery-platform/internal/platform/clock"
)

const (
	jwksMinRefetch = time.Minute   // an unknown key id cannot make us hammer the provider
	jwksMaxAge     = 6 * time.Hour // refresh even without an unknown kid
	jwksMaxBody    = 1 << 20
)

// OIDCProvider configures how one identity provider's ID tokens are validated.
type OIDCProvider struct {
	Issuers     []string // accepted `iss` values
	Audiences   []string // accepted `aud` values (our client ids)
	JWKSURL     string
	NonceSHA256 bool // Apple sends the hex SHA-256 of the nonce; Google sends it as given
}

type jwks struct {
	keys    map[string]*rsa.PublicKey
	fetched time.Time
}

// OIDCVerifier validates Google and Apple ID tokens against the provider's JWKS: signature (RS256 only), issuer,
// audience, expiry and nonce.
type OIDCVerifier struct {
	clk       clock.Clock
	hc        *http.Client
	providers map[string]OIDCProvider
	mu        sync.Mutex
	cache     map[string]*jwks
}

// NewOIDCVerifier returns a verifier for the configured providers.
func NewOIDCVerifier(clk clock.Clock, hc *http.Client, providers map[string]OIDCProvider) *OIDCVerifier {
	return &OIDCVerifier{clk: clk, hc: hc, providers: providers, cache: map[string]*jwks{}}
}

// VerifyIDToken implements IDTokenVerifier.
func (v *OIDCVerifier) VerifyIDToken(ctx context.Context, provider, idToken, nonce string) (string, error) {
	p, ok := v.providers[provider]
	if !ok {
		return "", fmt.Errorf("oidc: unknown provider %q", provider)
	}
	if nonce == "" {
		return "", errors.New("oidc: nonce required")
	}
	now, err := v.clk.Now(ctx, nil)
	if err != nil {
		return "", err
	}
	claims := jwt.MapClaims{}
	_, err = jwt.ParseWithClaims(idToken, claims, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		return v.key(ctx, provider, p, kid, now)
	}, jwt.WithValidMethods([]string{"RS256"}), jwt.WithExpirationRequired(), jwt.WithTimeFunc(func() time.Time { return now }))
	if err != nil {
		return "", fmt.Errorf("oidc: %w", err)
	}
	iss, _ := claims.GetIssuer()
	if !slices.Contains(p.Issuers, iss) {
		return "", errors.New("oidc: unexpected issuer")
	}
	aud, _ := claims.GetAudience()
	if !slices.ContainsFunc(aud, func(a string) bool { return slices.Contains(p.Audiences, a) }) {
		return "", errors.New("oidc: unexpected audience")
	}
	want := nonce
	if p.NonceSHA256 {
		h := sha256.Sum256([]byte(nonce))
		want = hex.EncodeToString(h[:])
	}
	if got, _ := claims["nonce"].(string); got == "" || got != want {
		return "", errors.New("oidc: nonce mismatch")
	}
	sub, _ := claims.GetSubject()
	if sub == "" {
		return "", errors.New("oidc: missing subject")
	}
	return sub, nil
}

func (v *OIDCVerifier) key(ctx context.Context, name string, p OIDCProvider, kid string, now time.Time) (*rsa.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	c := v.cache[name]
	if c != nil {
		if k, ok := c.keys[kid]; ok && now.Sub(c.fetched) < jwksMaxAge {
			return k, nil
		}
		if now.Sub(c.fetched) < jwksMinRefetch {
			return nil, errors.New("oidc: unknown key id")
		}
	}
	fresh, err := v.fetch(ctx, p.JWKSURL, now)
	if err != nil {
		return nil, err
	}
	v.cache[name] = fresh
	k, ok := fresh.keys[kid]
	if !ok {
		return nil, errors.New("oidc: unknown key id")
	}
	return k, nil
}

func (v *OIDCVerifier) fetch(ctx context.Context, url string, now time.Time) (*jwks, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := v.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oidc: fetch jwks: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("oidc: jwks status %d", resp.StatusCode)
	}
	var doc struct {
		Keys []struct{ Kty, Kid, N, E string } `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, jwksMaxBody)).Decode(&doc); err != nil {
		return nil, fmt.Errorf("oidc: decode jwks: %w", err)
	}
	out := &jwks{keys: map[string]*rsa.PublicKey{}, fetched: now}
	for _, k := range doc.Keys {
		if k.Kty != "RSA" || k.Kid == "" {
			continue
		}
		n, err1 := base64.RawURLEncoding.DecodeString(k.N)
		e, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil {
			continue
		}
		out.keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	return out, nil
}
