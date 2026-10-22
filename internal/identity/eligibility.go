package identity

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/itsivali/lottery-platform/internal/identity/store"
)

func (s *Service) loadActive(ctx context.Context, tx pgx.Tx, userID string) (store.GetUserRow, error) {
	u, err := store.New(tx).GetUser(ctx, userID)
	if err != nil {
		return u, err
	}
	if u.Status != "active" {
		return u, ErrAccountSuspended
	}
	return u, nil
}

// AssertCanTopUp refuses accounts that may not start a top-up: suspended or closed accounts and anyone whose national
// ID is not verified (which also keeps under-18s out, since they can never be verified). The phone need not be proved
// yet: the first top-up is what proves it. It returns the number the account claims, which is where the payment
// prompt must go (never a number the client supplies). Called inside the same transaction as the top-up (invariant 12).
func (s *Service) AssertCanTopUp(ctx context.Context, tx pgx.Tx, userID string) (string, error) {
	u, err := s.loadActive(ctx, tx, userID)
	if err != nil {
		return "", err
	}
	if u.KycStatus != "verified" {
		return "", ErrKYCRequired
	}
	return u.Msisdn, nil
}

// AssertCanPlay is AssertCanTopUp plus a proved phone number, which is what pays out and receives alerts. Called inside
// the same transaction as the debit.
func (s *Service) AssertCanPlay(ctx context.Context, tx pgx.Tx, userID string) error {
	u, err := s.loadActive(ctx, tx, userID)
	if err != nil {
		return err
	}
	if u.KycStatus != "verified" {
		return ErrKYCRequired
	}
	if !u.PhoneVerified {
		return ErrPhoneNotVerified
	}
	return nil
}

// AssertDeviceTrusted refuses transactions from a session whose device failed attestation. With strict set (the
// production policy once the verifier is live) a mobile device must also have passed; web sessions are not attestable
// and are limited by IP, CAPTCHA and lower rate limits instead.
func (s *Service) AssertDeviceTrusted(ctx context.Context, tx pgx.Tx, sessionID string, strict bool) error {
	d, err := store.New(tx).GetSessionDevice(ctx, sessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // a session without a device record is a web session
	}
	if err != nil {
		return err
	}
	if d.Platform == "web" {
		return nil
	}
	if d.AttestationVerdict == "failed" || (strict && d.AttestationVerdict != "passed") {
		return ErrAttestationFailed
	}
	return nil
}
