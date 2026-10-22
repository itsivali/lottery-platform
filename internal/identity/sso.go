package identity

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/itsivali/lottery-platform/internal/audit"
	"github.com/itsivali/lottery-platform/internal/identity/store"
)

func (s *Service) verifyIDToken(ctx context.Context, provider, idToken, nonce string) (string, error) {
	sub, err := s.oidc.VerifyIDToken(ctx, provider, idToken, nonce)
	if err != nil || sub == "" {
		return "", errors.Join(ErrTokenInvalid, err)
	}
	return sub, nil
}

// SignInSSO signs in an already-linked Google or Apple identity. An unlinked identity must first sign in with phone
// and password, complete KYC and then link (SSO never creates an account).
func (s *Service) SignInSSO(ctx context.Context, provider, idToken, nonce string, dev Device) (Tokens, error) {
	sub, err := s.verifyIDToken(ctx, provider, idToken, nonce)
	if err != nil {
		return Tokens{}, err
	}
	verdict := s.verdict(ctx, dev)
	var tokens Tokens
	err = s.withTx(ctx, func(_ pgx.Tx, q *store.Queries, now time.Time) error {
		userID, err := q.FindAuthIdentity(ctx, store.FindAuthIdentityParams{Provider: provider, Subject: sub})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrSSONotLinked
		}
		if err != nil {
			return err
		}
		if err := s.assertActive(ctx, q, userID); err != nil {
			return err
		}
		tokens, err = s.startSession(ctx, q, userID, dev, verdict, now)
		return err
	})
	return tokens, err
}

// LinkSSO attaches a Google or Apple identity to a phone- and KYC-verified account.
func (s *Service) LinkSSO(ctx context.Context, userID, provider, idToken, nonce string) error {
	return s.withTx(ctx, func(tx pgx.Tx, q *store.Queries, _ time.Time) error {
		u, err := q.GetUser(ctx, userID)
		if err != nil {
			return err
		}
		if u.KycStatus != "verified" {
			return ErrKYCRequired
		}
		if !u.PhoneVerified {
			return ErrPhoneNotVerified
		}
		sub, err := s.verifyIDToken(ctx, provider, idToken, nonce)
		if err != nil {
			return err
		}
		if err := q.InsertAuthIdentity(ctx, store.InsertAuthIdentityParams{UserID: userID, Provider: provider, Subject: sub}); err != nil {
			var pe *pgconn.PgError
			if errors.As(err, &pe) && pe.Code == "23505" {
				return ErrSSOAlreadyLinked
			}
			return err
		}
		return s.audit.Record(ctx, tx, audit.Event{ActorType: audit.System, Action: "sso.linked", EntityType: "user", EntityID: userID,
			Details: map[string]any{"provider": provider}})
	})
}
