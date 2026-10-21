// Package identity owns players' phone identity: OTP sign-in, access and refresh tokens, sessions, devices and SSO
// linking. National-ID verification lives in kyc; the ID, not the phone, is the identity of record.
package identity

import (
	"context"
	"crypto/ed25519"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/itsivali/lottery-platform/internal/audit"
	"github.com/itsivali/lottery-platform/internal/identity/store"
	"github.com/itsivali/lottery-platform/internal/platform/clock"
)

// OTP purposes.
const (
	PurposeLogin        = "login"
	PurposePayoutChange = "payout_change"
	PurposeSSOLink      = "sso_link"
)

// Errors. Handlers map them to HTTP statuses; OTP failures are deliberately indistinguishable to avoid oracles.
var (
	ErrOTPInvalid        = errors.New("identity: invalid or expired code")
	ErrTokenInvalid      = errors.New("identity: invalid token")
	ErrTokenReuse        = errors.New("identity: refresh token reuse detected")
	ErrDeviceCap         = errors.New("identity: too many accounts on this device")
	ErrDeviceRequired    = errors.New("identity: device fingerprint required")
	ErrAccountSuspended  = errors.New("identity: account suspended")
	ErrSSONotLinked      = errors.New("identity: no account is linked to this identity")
	ErrSSOAlreadyLinked  = errors.New("identity: this identity is already linked")
	ErrKYCRequired       = errors.New("identity: KYC must be verified first")
	ErrAttestationFailed = errors.New("identity: this device failed attestation")
)

// CooldownError reports that a new OTP cannot be requested yet.
type CooldownError struct{ RetryAfter time.Duration }

func (e *CooldownError) Error() string {
	return "identity: wait " + e.RetryAfter.Round(time.Second).String() + " before requesting another code"
}

// SMS sends text messages (the real adapter lives in notifications).
type SMS interface {
	Send(ctx context.Context, msisdn, text string) error
}

// IDTokenVerifier validates a Google or Apple ID token (issuer, audience, expiry, signature, nonce) and returns the
// stable subject.
type IDTokenVerifier interface {
	VerifyIDToken(ctx context.Context, provider, idToken, nonce string) (subject string, err error)
}

// Config carries secrets and tunables; secrets come from the deploy-time secret store.
type Config struct {
	OTPKey      []byte // HMAC key for OTP codes and device fingerprints (32+ bytes)
	TokenKey    ed25519.PrivateKey
	Issuer      string
	Audience    string
	AccessTTL   time.Duration // 10 minutes
	RefreshTTL  time.Duration
	OTPTTL      time.Duration // 5 minutes
	OTPCooldown time.Duration // 60 seconds
	DeviceCap   int           // accounts allowed per device (DECISION, default 2)
}

// Device describes the calling device. AttestationToken is the opaque Play Integrity / App Attest token the app
// obtained; it is only ever an input to the server-side Attestor, never a verdict.
type Device struct {
	Fingerprint      string
	Platform         string // android | ios | web
	AttestationToken string
}

// Attestor verifies a device attestation token server-side (Play Integrity on Android, App Attest on iOS). nonce binds
// the token to this sign-in. It reports whether the device passed; an error means the verifier could not decide.
type Attestor interface {
	Verify(ctx context.Context, platform, token, nonce string) (passed bool, err error)
}

// UnavailableAttestor is the default until a real verifier is configured: it never passes a device.
type UnavailableAttestor struct{}

// Verify implements Attestor.
func (UnavailableAttestor) Verify(context.Context, string, string, string) (bool, error) {
	return false, errors.New("identity: no attestation verifier configured")
}

// Tokens are returned on sign-in and refresh.
type Tokens struct {
	Access        string
	Refresh       string
	AccessExpires time.Time
}

// Principal is an authenticated caller.
type Principal struct{ UserID, SessionID string }

// Service implements sign-in and session management.
type Service struct {
	pool  *pgxpool.Pool
	clk   clock.Clock
	sms   SMS
	oidc  IDTokenVerifier
	audit *audit.Writer
	att   Attestor
	cfg   Config
}

// NewService returns a Service.
func NewService(pool *pgxpool.Pool, clk clock.Clock, sms SMS, oidc IDTokenVerifier, a *audit.Writer, cfg Config) *Service {
	return &Service{pool: pool, clk: clk, sms: sms, oidc: oidc, audit: a, att: UnavailableAttestor{}, cfg: cfg}
}

// WithAttestor installs the device attestation verifier (it must be set before the service handles requests).
func (s *Service) WithAttestor(a Attestor) *Service {
	s.att = a
	return s
}

// verdict asks the attestor about a mobile device before any transaction is opened. Without a token, on web, or when
// the verifier cannot decide, the verdict is "none" (unknown), never "passed".
func (s *Service) verdict(ctx context.Context, dev Device) string {
	if (dev.Platform != "android" && dev.Platform != "ios") || dev.AttestationToken == "" {
		return "none"
	}
	passed, err := s.att.Verify(ctx, dev.Platform, dev.AttestationToken, dev.Fingerprint)
	switch {
	case err != nil:
		return "none"
	case passed:
		return "passed"
	}
	return "failed"
}

func (s *Service) now(ctx context.Context, q clock.Querier) (time.Time, error) {
	return s.clk.Now(ctx, q)
}

// withTx runs fn in a transaction with the transaction's business time.
func (s *Service) withTx(ctx context.Context, fn func(tx pgx.Tx, q *store.Queries, now time.Time) error) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		now, err := s.now(ctx, tx)
		if err != nil {
			return err
		}
		return fn(tx, store.New(tx), now)
	})
}
