package identity

import (
	"context"

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
