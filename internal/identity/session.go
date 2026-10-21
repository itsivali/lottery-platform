package identity

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"

	"github.com/itsivali/lottery-platform/internal/audit"
	"github.com/itsivali/lottery-platform/internal/identity/store"
	"github.com/itsivali/lottery-platform/internal/platform/crypto"
)

type accessClaims struct {
	SessionID string `json:"sid"`
	jwt.RegisteredClaims
}

func refreshHash(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// issue creates an access token and a new refresh token for the session. parentID chains a rotation.
func (s *Service) issue(ctx context.Context, q *store.Queries, userID, sessionID string, parentID *string, now time.Time) (Tokens, error) {
	exp := now.Add(s.cfg.AccessTTL)
	access, err := jwt.NewWithClaims(jwt.SigningMethodEdDSA, accessClaims{
		SessionID: sessionID,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: s.cfg.Issuer, Audience: jwt.ClaimStrings{s.cfg.Audience}, Subject: userID,
			IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(exp),
		},
	}).SignedString(s.cfg.TokenKey)
	if err != nil {
		return Tokens{}, err
	}
	refresh, err := crypto.RandomToken(32)
	if err != nil {
		return Tokens{}, err
	}
	if err := q.InsertRefreshToken(ctx, store.InsertRefreshTokenParams{
		SessionID: sessionID, ParentID: parentID, TokenHash: refreshHash(refresh), IssuedAt: now, ExpiresAt: now.Add(s.cfg.RefreshTTL),
	}); err != nil {
		return Tokens{}, err
	}
	return Tokens{Access: access, Refresh: refresh, AccessExpires: exp}, nil
}

// startSession registers the device, opens a session and issues the first tokens.
func (s *Service) startSession(ctx context.Context, q *store.Queries, userID string, dev Device, now time.Time) (Tokens, error) {
	deviceID, err := s.registerDevice(ctx, q, userID, dev, now)
	if err != nil {
		return Tokens{}, err
	}
	sid, err := q.InsertSession(ctx, store.InsertSessionParams{UserID: userID, DeviceID: deviceID, CreatedAt: now})
	if err != nil {
		return Tokens{}, err
	}
	return s.issue(ctx, q, userID, sid, nil, now)
}

// VerifyOTP completes a phone sign-in, creating the account on first use.
func (s *Service) VerifyOTP(ctx context.Context, rawMSISDN, code string, dev Device) (Tokens, error) {
	msisdn, err := NormalizeMSISDN(rawMSISDN)
	if err != nil {
		return Tokens{}, ErrOTPInvalid
	}
	var tokens Tokens
	var outcome error
	err = s.withTx(ctx, func(tx pgx.Tx, q *store.Queries, now time.Time) error {
		ok, err := s.checkOTP(ctx, q, now, msisdn, PurposeLogin, code)
		if err != nil {
			return err
		}
		if !ok {
			outcome = ErrOTPInvalid // commit so the failed attempt is counted
			return nil
		}
		u, err := q.InsertUser(ctx, msisdn)
		if err != nil {
			return err
		}
		if err := s.assertActive(ctx, q, u.ID); err != nil {
			return err
		}
		if u.Created {
			if err := s.audit.Record(ctx, tx, audit.Event{ActorType: audit.System, Action: "user.created", EntityType: "user", EntityID: u.ID}); err != nil {
				return err
			}
		}
		tokens, err = s.startSession(ctx, q, u.ID, dev, now)
		return err
	})
	if err != nil {
		return Tokens{}, err
	}
	return tokens, outcome
}

func (s *Service) assertActive(ctx context.Context, q *store.Queries, userID string) error {
	u, err := q.GetUser(ctx, userID)
	if err != nil {
		return err
	}
	if u.Status != "active" {
		return ErrAccountSuspended
	}
	return nil
}

// Refresh rotates a refresh token. Presenting an already-used token revokes the whole session; so does presenting a
// token from a different device than the one the session is bound to.
func (s *Service) Refresh(ctx context.Context, token, fingerprint string) (Tokens, error) {
	var tokens Tokens
	var outcome error
	err := s.withTx(ctx, func(tx pgx.Tx, q *store.Queries, now time.Time) error {
		rt, err := q.LockRefreshToken(ctx, refreshHash(token))
		if errors.Is(err, pgx.ErrNoRows) {
			outcome = ErrTokenInvalid
			return nil
		}
		if err != nil {
			return err
		}
		sess, err := q.GetSession(ctx, rt.SessionID)
		if err != nil {
			return err
		}
		if sess.RevokedAt != nil {
			outcome = ErrTokenInvalid
			return nil
		}
		revoke := func(action string, result error) error {
			outcome = result
			if err := q.RevokeSession(ctx, store.RevokeSessionParams{ID: sess.ID, At: &now, Reason: ptr("reuse_detected")}); err != nil {
				return err
			}
			return s.audit.Record(ctx, tx, audit.Event{ActorType: audit.System, Action: action, EntityType: "session", EntityID: sess.ID,
				Details: map[string]any{"user_id": sess.UserID}})
		}
		if rt.UsedAt != nil {
			return revoke("auth.refresh_reuse", ErrTokenReuse)
		}
		if !now.Before(rt.ExpiresAt) {
			outcome = ErrTokenInvalid
			return nil
		}
		if sess.FingerprintHash != nil && !crypto.Equal(sess.FingerprintHash, s.fingerprintHash(fingerprint)) {
			return revoke("auth.refresh_device_mismatch", ErrTokenInvalid)
		}
		if err := s.assertActive(ctx, q, sess.UserID); err != nil {
			return err
		}
		if err := q.MarkRefreshUsed(ctx, store.MarkRefreshUsedParams{ID: rt.ID, UsedAt: &now}); err != nil {
			return err
		}
		tokens, err = s.issue(ctx, q, sess.UserID, sess.ID, &rt.ID, now)
		return err
	})
	if err != nil {
		return Tokens{}, err
	}
	return tokens, outcome
}

// Logout ends a session.
func (s *Service) Logout(ctx context.Context, sessionID string) error {
	return s.withTx(ctx, func(_ pgx.Tx, q *store.Queries, now time.Time) error {
		return q.RevokeSession(ctx, store.RevokeSessionParams{ID: sessionID, At: &now, Reason: ptr("logout")})
	})
}

// Authenticate validates an access token and that its session is still live.
func (s *Service) Authenticate(ctx context.Context, token string) (Principal, error) {
	if token == "" {
		return Principal{}, ErrTokenInvalid
	}
	now, err := s.now(ctx, s.pool)
	if err != nil {
		return Principal{}, err
	}
	var claims accessClaims
	pub := s.cfg.TokenKey.Public().(ed25519.PublicKey) //nolint:forcetypeassert // ed25519 private keys always yield ed25519 public keys
	_, err = jwt.ParseWithClaims(token, &claims, func(*jwt.Token) (any, error) { return pub, nil },
		jwt.WithValidMethods([]string{"EdDSA"}), jwt.WithIssuer(s.cfg.Issuer), jwt.WithAudience(s.cfg.Audience),
		jwt.WithExpirationRequired(), jwt.WithTimeFunc(func() time.Time { return now }))
	if err != nil || claims.Subject == "" || claims.SessionID == "" {
		return Principal{}, ErrTokenInvalid
	}
	sess, err := store.New(s.pool).GetSession(ctx, claims.SessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Principal{}, ErrTokenInvalid
	}
	if err != nil {
		return Principal{}, err
	}
	if sess.RevokedAt != nil || sess.UserID != claims.Subject {
		return Principal{}, ErrTokenInvalid
	}
	return Principal{UserID: claims.Subject, SessionID: claims.SessionID}, nil
}

func ptr[T any](v T) *T { return &v }
