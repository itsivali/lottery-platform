// Package app builds the running services from configuration. Both cmd/api and cmd/worker use it, so the wiring that
// production runs is the wiring the tests exercise.
package app

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/valkey-io/valkey-go"

	"github.com/itsivali/lottery-platform/internal/api"
	"github.com/itsivali/lottery-platform/internal/audit"
	"github.com/itsivali/lottery-platform/internal/game"
	"github.com/itsivali/lottery-platform/internal/identity"
	"github.com/itsivali/lottery-platform/internal/kyc"
	"github.com/itsivali/lottery-platform/internal/ledger"
	"github.com/itsivali/lottery-platform/internal/payments"
	"github.com/itsivali/lottery-platform/internal/payments/daraja"
	"github.com/itsivali/lottery-platform/internal/platform/clock"
	"github.com/itsivali/lottery-platform/internal/platform/config"
	"github.com/itsivali/lottery-platform/internal/platform/crypto"
	"github.com/itsivali/lottery-platform/internal/platform/db"
	"github.com/itsivali/lottery-platform/internal/platform/idempotency"
	"github.com/itsivali/lottery-platform/internal/platform/ratelimit"
	"github.com/itsivali/lottery-platform/internal/rg"
	"github.com/itsivali/lottery-platform/internal/wallet"
)

const (
	issuer   = "lottery-platform"
	audience = "lottery-api"
)

// App is the assembled application.
type App struct {
	// Mount registers the /v1 routes; it is nil when the API is not configured (health probes only).
	Mount func(chi.Router)

	cfg      config.Config
	pool     *pgxpool.Pool
	valkey   valkey.Client
	kyc      *kyc.Service
	payments *payments.Service
	live     *payments.Live
	stopLive context.CancelFunc
	workers  *river.Client[pgx.Tx]
}

// New builds the application. With the API unconfigured it returns an App that serves nothing beyond the probes.
func New(ctx context.Context, cfg config.Config, pool *pgxpool.Pool) (*App, error) {
	a := &App{cfg: cfg, pool: pool}
	if !cfg.API.Enabled {
		return a, nil
	}
	c := cfg.API
	hc := &http.Client{Timeout: 20 * time.Second}

	vk, err := newValkey(c.ValkeyAddr)
	if err != nil {
		return nil, fmt.Errorf("app: valkey: %w", err)
	}
	a.valkey = vk

	keyring, err := crypto.NewKeyring(c.DataKeyActive, c.DataKeys)
	if err != nil {
		return nil, err
	}
	aw := audit.NewWriter()
	clk := clock.Postgres{}

	// Anything not configured fails closed instead of pretending to work.
	// No registry by default: identities are reviewed by staff (ADR 0009). A registry gateway can still be plugged in.
	var provider kyc.Provider = kyc.ManualProvider{}
	if c.KYCURL != "" {
		provider = kyc.NewHTTPProvider(c.KYCURL, c.KYCKey, hc)
	}

	insertOnly, err := river.NewClient(riverpgxv5.New(pool), &river.Config{Schema: db.RiverSchema})
	if err != nil {
		return nil, err
	}
	idKey := c.IdentityKey
	a.kyc = kyc.NewService(pool, clk, provider, keyring, idKey, aw, kyc.NewRiverEnqueuer(insertOnly, 0))

	idSvc := identity.NewService(pool, clk, oidcVerifier(clock.Wall{}, hc, c), aw, identity.Config{
		AuthKey: c.AuthKey, TokenKey: ed25519.NewKeyFromSeed(c.TokenSeed), Issuer: issuer, Audience: audience,
		AccessTTL: 10 * time.Minute, RefreshTTL: 30 * 24 * time.Hour, DeviceCap: 2,
	})

	rgSvc := rg.NewService(pool, clk, aw, rg.Config{CoolingOff: 24 * time.Hour})

	// Without Daraja configuration the payments service still answers reads but refuses top-ups (fail closed).
	var dc payments.Daraja
	var client *daraja.Client
	if p := cfg.Payments; p.Enabled {
		client, err = NewDaraja(p)
		if err != nil {
			return nil, err
		}
		dc = client
	}
	ls := ledger.NewStore()
	a.payments = payments.NewService(pool, clk, dc, payments.NewRiverEnqueuer(insertOnly), idSvc, rgSvc, wallet.NewService(ls), ls, aw, payments.Config{})
	if p := cfg.Payments; p.C2BEnabled() {
		a.payments.WithC2B(client, payments.C2BConfig{ShortCode: p.C2BShortCode})
	}

	gameSvc := game.NewService(pool, clk, idSvc, rgSvc, wallet.NewService(ls), keyring, wallet.ErrInsufficientFunds)

	// One connection per instance listens for payment changes and fans them out to the players' streams.
	listenCfg := pool.Config().ConnConfig.Copy()
	a.live = payments.NewLive(func(ctx context.Context) (*pgx.Conn, error) { return pgx.ConnectConfig(ctx, listenCfg) }, payments.LiveConfig{})
	liveCtx, stopLive := context.WithCancel(context.WithoutCancel(ctx))
	a.stopLive = stopLive
	go a.live.Run(liveCtx)

	a.Mount = api.Mount(api.Deps{
		Identity: idSvc, KYC: a.kyc, RG: rgSvc, Payments: a.payments, Game: gameSvc, Live: a.live, CallbackPath: cfg.Payments.CallbackPath,
		Idem:               idempotency.NewStore(pool),
		Limiter:            ratelimit.New(vk),
		TrustedProxyHeader: c.TrustedProxyHeader, TrustedProxyHops: c.TrustedProxyHops,
	})
	slog.InfoContext(ctx, "v1 API enabled", "trusted_proxy_header", c.TrustedProxyHeader, "trusted_proxy_hops", c.TrustedProxyHops,
		"kyc_registry", c.KYCURL != "", "google", c.GoogleClientID != "", "apple", c.AppleClientID != "",
		"payments", cfg.Payments.Enabled, "buy_goods", cfg.Payments.TillNumber != "", "paybill", cfg.Payments.C2BEnabled(), "daraja", cfg.Payments.BaseURL)
	return a, nil
}

// NewDaraja builds the Daraja client from configuration, with the paybill calls when C2B is configured. lotteryctl
// uses it too, to register the paybill URLs.
func NewDaraja(p config.Payments) (*daraja.Client, error) {
	client, err := daraja.New(daraja.Config{
		BaseURL: p.BaseURL, ConsumerKey: p.ConsumerKey, ConsumerSecret: p.ConsumerSecret, ShortCode: p.ShortCode,
		Passkey: p.Passkey, TillNumber: p.TillNumber, CallbackURL: p.CallbackURL(), AllowProduction: p.AllowProduction,
	}, &http.Client{Timeout: 35 * time.Second}, clock.Wall{})
	if err != nil || !p.C2BEnabled() {
		return client, err
	}
	return client.WithC2B(daraja.C2BConfig{ShortCode: p.C2BShortCode, Initiator: p.Initiator, SecurityCredential: p.SecurityCredential,
		ResultURL: p.C2BURL("status-result"), TimeoutURL: p.C2BURL("status-timeout")})
}

// StartWorkers starts the background job workers (a no-op when the API is not configured).
func (a *App) StartWorkers(ctx context.Context) error {
	if a.kyc == nil {
		return nil
	}
	workers := river.NewWorkers()
	// the worker side needs the verification service without the enqueuer; it shares provider, keys and pool
	kyc.AddWorkers(workers, a.kyc)
	payments.AddWorkers(workers, a.payments)
	wc, err := river.NewClient(riverpgxv5.New(a.pool), &river.Config{
		Schema: db.RiverSchema, Workers: workers, PeriodicJobs: payments.PeriodicJobs(),
		Queues: map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 10}},
	})
	if err != nil {
		return err
	}
	if err := wc.Start(ctx); err != nil {
		return err
	}
	a.workers = wc
	return nil
}

// StopStreams ends every open payment stream and refuses new ones. The server calls it when shutdown begins, since
// a stream would otherwise hold the shutdown until its client went away.
func (a *App) StopStreams() {
	if a.live != nil {
		a.live.Close()
	}
}

// Close stops the workers and releases clients.
func (a *App) Close(ctx context.Context) {
	a.StopStreams()
	if a.stopLive != nil {
		a.stopLive()
	}
	if a.workers != nil {
		_ = a.workers.Stop(ctx)
	}
	if a.valkey != nil {
		a.valkey.Close()
	}
}

func newValkey(addr string) (valkey.Client, error) {
	opt := valkey.ClientOption{InitAddress: []string{addr}, DisableCache: true}
	if strings.Contains(addr, "://") {
		parsed, err := valkey.ParseURL(addr)
		if err != nil {
			return nil, err
		}
		parsed.DisableCache = true
		opt = parsed
	}
	return valkey.NewClient(opt)
}

// refusingOIDC rejects every token: SSO stays off until a client id is configured.
type refusingOIDC struct{}

func (refusingOIDC) VerifyIDToken(context.Context, string, string, string) (string, error) {
	return "", errors.New("sso is not configured")
}

func oidcVerifier(clk clock.Clock, hc *http.Client, c config.API) identity.IDTokenVerifier {
	providers := map[string]identity.OIDCProvider{}
	if c.GoogleClientID != "" {
		providers["google"] = identity.OIDCProvider{
			Issuers: []string{"https://accounts.google.com", "accounts.google.com"}, Audiences: []string{c.GoogleClientID},
			JWKSURL: "https://www.googleapis.com/oauth2/v3/certs",
		}
	}
	if c.AppleClientID != "" {
		providers["apple"] = identity.OIDCProvider{
			Issuers: []string{"https://appleid.apple.com"}, Audiences: []string{c.AppleClientID},
			JWKSURL: "https://appleid.apple.com/auth/keys", NonceSHA256: true,
		}
	}
	if len(providers) == 0 {
		return refusingOIDC{}
	}
	return identity.NewOIDCVerifier(clk, hc, providers)
}
