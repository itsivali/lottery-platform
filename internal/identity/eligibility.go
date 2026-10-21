package identity

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/itsivali/lottery-platform/internal/identity/store"
)

// AssertEligible refuses accounts that may not transact: suspended or closed accounts and anyone whose national ID is
// not verified (which also keeps under-18s out, since they can never be verified). Top-ups and plays call it inside
// the same transaction as the money movement (invariant 12), alongside rg.AssertNotRestricted.
func (s *Service) AssertEligible(ctx context.Context, tx pgx.Tx, userID string) error {
	u, err := store.New(tx).GetUser(ctx, userID)
	if err != nil {
		return err
	}
	if u.Status != "active" {
		return ErrAccountSuspended
	}
	if u.KycStatus != "verified" {
		return ErrKYCRequired
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
