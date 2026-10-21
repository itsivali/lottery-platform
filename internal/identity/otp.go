package identity

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/itsivali/lottery-platform/internal/identity/store"
	"github.com/itsivali/lottery-platform/internal/platform/crypto"
)

const otpDigits = 6

func validPurpose(p string) bool {
	return p == PurposeLogin || p == PurposePayoutChange || p == PurposeSSOLink
}

// RequestOTP sends a fresh code. It never reveals whether the number already has an account. Per-number and per-IP
// request limits are applied by the rate-limit middleware in front of this call.
func (s *Service) RequestOTP(ctx context.Context, rawMSISDN, purpose, ip string) error {
	msisdn, err := NormalizeMSISDN(rawMSISDN)
	if err != nil {
		return err
	}
	if !validPurpose(purpose) {
		return fmt.Errorf("identity: unknown OTP purpose %q", purpose)
	}
	var code string
	err = s.withTx(ctx, func(_ pgx.Tx, q *store.Queries, now time.Time) error {
		last, err := q.LatestOTPCreatedAt(ctx, store.LatestOTPCreatedAtParams{Msisdn: msisdn, Purpose: purpose})
		switch {
		case err == nil:
			if wait := s.cfg.OTPCooldown - now.Sub(last); wait > 0 {
				return &CooldownError{RetryAfter: wait}
			}
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		code, err = crypto.RandomDigits(otpDigits)
		if err != nil {
			return err
		}
		var addr *netip.Addr
		if a, perr := netip.ParseAddr(ip); perr == nil {
			addr = &a
		}
		return q.InsertOTP(ctx, store.InsertOTPParams{
			Msisdn: msisdn, Purpose: purpose, CodeHmac: crypto.MAC(s.cfg.OTPKey, "otp", msisdn, purpose, code),
			ExpiresAt: now.Add(s.cfg.OTPTTL), Ip: addr, CreatedAt: now,
		})
	})
	if err != nil {
		return err
	}
	if err := s.sms.Send(ctx, msisdn, "Your verification code is "+code+". It expires in 5 minutes. Never share it."); err != nil {
		return fmt.Errorf("identity: send sms: %w", err)
	}
	return nil
}

// checkOTP verifies and consumes a code inside tx. A wrong code increments the attempt counter; the caller must
// commit in that case (ok=false, err=nil) so the failed attempt is recorded.
func (s *Service) checkOTP(ctx context.Context, q *store.Queries, now time.Time, msisdn, purpose, code string) (bool, error) {
	row, err := q.LockActiveOTP(ctx, store.LockActiveOTPParams{Msisdn: msisdn, Purpose: purpose, ExpiresAt: now})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	want := crypto.MAC(s.cfg.OTPKey, "otp", msisdn, purpose, code)
	if !crypto.Equal(want, row.CodeHmac) {
		return false, q.BumpOTPAttempts(ctx, row.ID)
	}
	return true, q.ConsumeOTP(ctx, store.ConsumeOTPParams{ID: row.ID, ConsumedAt: &now})
}
