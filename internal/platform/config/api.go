package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
)

// API is the configuration of the public /v1 API. It is all-or-nothing: with none of the secrets set the process
// serves only its health probes; with some but not all set it refuses to start, so a half-configured deployment can
// never expose a half-working (or half-secured) API.
type API struct {
	Enabled bool

	AuthKey       []byte          // HMAC root key: password pepper and device-fingerprint keys derive from it
	TokenSeed     []byte          // ed25519 seed for access tokens
	IdentityKey   []byte          // HMAC key for the one-account-per-national-ID lookup hash
	DataKeys      map[byte][]byte // key-encryption keys for KYC fields, by id
	DataKeyActive byte
	ValkeyAddr    string

	// TrustedProxyHeader is the header our load balancer sets with the client address; "" means use the socket address.
	// TrustedProxyHops is how many trusted proxies appended to X-Forwarded-For.
	TrustedProxyHeader string
	TrustedProxyHops   int

	KYCURL, KYCKey string

	GoogleClientID string
	AppleClientID  string
}

var apiSecrets = []string{"AUTH_KEY", "TOKEN_SEED", "DATA_KEYS", "IDENTITY_KEY", "VALKEY_ADDR"}

// dataKeysEnv returns the key list. DATA_KEY (one base64 key) is shorthand for "1:<key>" so hosts that can only generate
// a bare random value can supply it; rotation uses the explicit DATA_KEYS form.
func dataKeysEnv() string {
	if v := os.Getenv("DATA_KEYS"); v != "" {
		return v
	}
	if v := os.Getenv("DATA_KEY"); v != "" {
		return "1:" + v
	}
	return ""
}

func loadAPI() (API, error) {
	var set, missing []string
	for _, k := range apiSecrets {
		present := os.Getenv(k) != ""
		if k == "DATA_KEYS" {
			present = dataKeysEnv() != ""
		}
		if present {
			set = append(set, k)
		} else {
			missing = append(missing, k)
		}
	}
	if len(set) == 0 {
		return API{}, nil
	}
	if len(missing) > 0 {
		return API{}, fmt.Errorf("config: the API is partly configured (%s set) but %s missing", strings.Join(set, ", "), strings.Join(missing, ", "))
	}

	a := API{Enabled: true, ValkeyAddr: os.Getenv("VALKEY_ADDR"), TrustedProxyHops: 1,
		KYCURL: os.Getenv("KYC_API_URL"), KYCKey: os.Getenv("KYC_API_KEY"),
		GoogleClientID: os.Getenv("GOOGLE_CLIENT_ID"), AppleClientID: os.Getenv("APPLE_CLIENT_ID")}
	var err error
	if a.AuthKey, err = secret("AUTH_KEY", 32, false); err != nil {
		return API{}, err
	}
	if a.TokenSeed, err = secret("TOKEN_SEED", 32, true); err != nil {
		return API{}, err
	}
	if a.IdentityKey, err = secret("IDENTITY_KEY", 32, false); err != nil {
		return API{}, err
	}
	if err := a.loadDataKeys(); err != nil {
		return API{}, err
	}

	switch h := os.Getenv("TRUSTED_PROXY_HEADER"); h {
	case "":
		return API{}, errors.New("config: TRUSTED_PROXY_HEADER must be set (the header your load balancer sets, e.g. X-Forwarded-For; use 'none' only when there is no proxy), otherwise per-IP rate limits would key on the balancer's address")
	case "none":
	default:
		a.TrustedProxyHeader = h
	}
	if v := os.Getenv("TRUSTED_PROXY_HOPS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 5 {
			return API{}, errors.New("config: TRUSTED_PROXY_HOPS must be an integer from 1 to 5")
		}
		a.TrustedProxyHops = n
	}
	for _, g := range []struct{ name, val string }{{"KYC_API_URL", a.KYCURL}} {
		if err := checkGatewayURL(g.name, g.val); err != nil {
			return API{}, err
		}
	}
	return a, nil
}

// secret decodes a base64 variable. exact requires exactly n bytes, otherwise at least n.
func secret(name string, n int, exact bool) ([]byte, error) {
	b, err := base64.StdEncoding.DecodeString(os.Getenv(name))
	if err != nil {
		return nil, fmt.Errorf("config: %s is not valid base64", name)
	}
	if (exact && len(b) != n) || len(b) < n {
		return nil, fmt.Errorf("config: %s must decode to %s%d bytes", name, map[bool]string{true: "exactly ", false: "at least "}[exact], n)
	}
	return b, nil
}

// loadDataKeys parses DATA_KEYS ("1:<base64>,2:<base64>") and DATA_KEY_ACTIVE (default: the highest id).
func (a *API) loadDataKeys() error {
	a.DataKeys = map[byte][]byte{}
	for _, part := range strings.Split(dataKeysEnv(), ",") {
		idStr, enc, ok := strings.Cut(strings.TrimSpace(part), ":")
		id, err := strconv.ParseUint(idStr, 10, 8)
		if !ok || err != nil {
			return errors.New("config: DATA_KEYS must look like 1:<base64>[,2:<base64>]")
		}
		key, err := base64.StdEncoding.DecodeString(enc)
		if err != nil || len(key) != 32 {
			return errors.New("config: DATA_KEYS entries must be base64 of exactly 32 bytes")
		}
		if _, dup := a.DataKeys[byte(id)]; dup {
			return fmt.Errorf("config: DATA_KEYS repeats key id %d", id)
		}
		a.DataKeys[byte(id)] = key
	}
	ids := make([]byte, 0, len(a.DataKeys))
	for id := range a.DataKeys {
		ids = append(ids, id)
	}
	a.DataKeyActive = slices.Max(ids)
	if v := os.Getenv("DATA_KEY_ACTIVE"); v != "" {
		n, err := strconv.ParseUint(v, 10, 8)
		if err != nil || a.DataKeys[byte(n)] == nil {
			return errors.New("config: DATA_KEY_ACTIVE must be the id of a key in DATA_KEYS")
		}
		a.DataKeyActive = byte(n)
	}
	return nil
}

// checkGatewayURL requires https for third-party gateways; plain http is allowed only for local or in-cluster hosts
// (names without a dot, localhost, loopback), which is how the Docker Compose mocks are reached.
func checkGatewayURL(name, raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("config: %s is not a valid URL", name)
	}
	if u.Scheme == "https" {
		return nil
	}
	host := u.Hostname()
	if u.Scheme == "http" && (host == "localhost" || host == "127.0.0.1" || !strings.Contains(host, ".")) {
		return nil
	}
	return fmt.Errorf("config: %s must use https (plain http is only allowed for local hosts)", name)
}
