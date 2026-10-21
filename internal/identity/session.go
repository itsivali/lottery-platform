package identity

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/itsivali/lottery-platform/internal/audit"
	"github.com/itsivali/lottery-platform/internal/identity/store"
	"github.com/itsivali/lottery-platform/internal/platform/crypto"
	"github.com/itsivali/lottery-platform/internal/platform/password"
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
func (s *Service) startSession(ctx context.Context, q *store.Queries, userID string, dev Device, verdict string, now time.Time) (Tokens, error) {
	deviceID, err := s.registerDevice(ctx, q, userID, dev, verdict, now)
	if err != nil {
		return Tokens{}, err
	}
	sid, err := q.InsertSession(ctx, store.InsertSessionParams{UserID: userID, DeviceID: deviceID, CreatedAt: now})
	if err != nil {
		return Tokens{}, err
	}
	return s.issue(ctx, q, userID, sid, nil, now)
}

// maxPasswordBytes bounds what Login will hash, so a huge "password" cannot be used to burn CPU.
const maxPasswordBytes = 1024

// Signup creates an account from a phone number and password and signs it in. The number is only a claim until the
// player proves it with an M-Pesa confirmation, so several people may claim one number; a number that already has a
// proved owner cannot be claimed again.
func (s *Service) Signup(ctx context.Context, rawMSISDN, pw string, dev Device) (Tokens, error) {
	msisdn, err := NormalizeMSISDN(rawMSISDN)
	if err != nil {
		return Tokens{}, err
	}
	if err := password.CheckPolicy(pw, msisdn); err != nil {
		return Tokens{}, err
	}
	hash, err := password.Hash(pw, s.pepper)
	if err != nil {
		return Tokens{}, err
	}
	verdict := s.verdict(ctx, dev)
	var tokens Tokens
	err = s.withTx(ctx, func(tx pgx.Tx, q *store.Queries, now time.Time) error {
		taken, err := q.VerifiedAccountExists(ctx, msisdn)
		if err != nil {
			return err
		}
		if taken {
			return ErrPhoneRegistered
		}
		id, err := q.InsertAccount(ctx, store.InsertAccountParams{Msisdn: msisdn, PasswordHash: &hash, At: &now})
		if err != nil {
			return err
		}
		if err := s.audit.Record(ctx, tx, audit.Event{ActorType: audit.System, Action: "user.created", EntityType: "user", EntityID: id}); err != nil {
			return err
		}
		tokens, err = s.startSession(ctx, q, id, dev, verdict, now)
		return err
	})
	return tokens, err
}

// Login checks a phone number and password. Every failure to authenticate returns the same error and costs the same
// time, so the response does not reveal whether a number has an account.
func (s *Service) Login(ctx context.Context, rawMSISDN, pw string, dev Device) (Tokens, error) {
	msisdn, nerr := NormalizeMSISDN(rawMSISDN)
	if nerr != nil || len(pw) > maxPasswordBytes {
		_, _ = password.Verify(pw[:min(len(pw), maxPasswordBytes)], s.dummyHash, s.pepper)
		return Tokens{}, ErrInvalidCredentials
	}
	cands, err := store.New(s.pool).LoginCandidates(ctx, msisdn)
	if err != nil {
		return Tokens{}, err
	}
	if len(cands) > 0 && cands[0].PhoneVerified {
		cands = cands[:1] // the proved owner is the only one who may use this number
	}
	var matched *store.LoginCandidatesRow
	for i := range cands {
		if ok, verr := password.Verify(pw, *cands[i].PasswordHash, s.pepper); verr == nil && ok {
			matched = &cands[i]
			break
		}
	}
	if len(cands) == 0 {
		_, _ = password.Verify(pw, s.dummyHash, s.pepper)
	}
	if matched == nil {
		return Tokens{}, ErrInvalidCredentials
	}
	if matched.Status != "active" {
		return Tokens{}, ErrAccountSuspended
	}
	upgraded := ""
	if password.NeedsRehash(*matched.PasswordHash) {
		upgraded, _ = password.Hash(pw, s.pepper)
	}
	verdict := s.verdict(ctx, dev)
	var tokens Tokens
	err = s.withTx(ctx, func(_ pgx.Tx, q *store.Queries, now time.Time) error {
		if upgraded != "" {
			if err := q.SetPassword(ctx, store.SetPasswordParams{ID: matched.ID, PasswordHash: &upgraded, PasswordChangedAt: &now}); err != nil {
				return err
			}
		}
		var err error
		tokens, err = s.startSession(ctx, q, matched.ID, dev, verdict, now)
		return err
	})
	return tokens, err
}

// ChangePassword replaces the password after checking the current one, and ends every other session.
func (s *Service) ChangePassword(ctx context.Context, userID, sessionID, current, next string) error {
	u, err := store.New(s.pool).GetUser(ctx, userID)
	if err != nil {
		return err
	}
	if u.PasswordHash == nil || len(current) > maxPasswordBytes {
		return ErrInvalidCredentials
	}
	if ok, verr := password.Verify(current, *u.PasswordHash, s.pepper); verr != nil || !ok {
		return ErrInvalidCredentials
	}
	if err := password.CheckPolicy(next, u.Msisdn); err != nil {
		return err
	}
	hash, err := password.Hash(next, s.pepper)
	if err != nil {
		return err
	}
	return s.withTx(ctx, func(tx pgx.Tx, q *store.Queries, now time.Time) error {
		if err := q.SetPassword(ctx, store.SetPasswordParams{ID: userID, PasswordHash: &hash, PasswordChangedAt: &now}); err != nil {
			return err
		}
		if err := q.RevokeOtherSessions(ctx, store.RevokeOtherSessionsParams{At: &now, UserID: userID, KeepSessionID: sessionID}); err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, audit.Event{ActorType: audit.Player, ActorID: userID, Action: "auth.password_changed", EntityType: "user", EntityID: userID})
	})
}

// MarkPhoneVerified records that the player proved they control msisdn. The payments module calls it, inside the
// transaction that verifies an STK confirmation, with the number the confirmation names (never one the client supplied).
// It is idempotent for the same account; if another account proved the number first it returns ErrPhoneTaken.
func (s *Service) MarkPhoneVerified(ctx context.Context, tx pgx.Tx, userID, confirmedMSISDN string) error {
	q := store.New(tx)
	row, err := q.LockUserPhone(ctx, userID)
	if err != nil {
		return err
	}
	if row.Msisdn != confirmedMSISDN {
		return ErrPhoneMismatch
	}
	if row.Verified {
		return nil
	}
	now, err := s.now(ctx, tx)
	if err != nil {
		return err
	}
	if err := q.MarkPhoneVerified(ctx, store.MarkPhoneVerifiedParams{ID: userID, MsisdnVerifiedAt: &now}); err != nil {
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "23505" {
			return ErrPhoneTaken
		}
		return err
	}
	return s.audit.Record(ctx, tx, audit.Event{ActorType: audit.System, Action: "user.phone_verified", EntityType: "user", EntityID: userID})
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
