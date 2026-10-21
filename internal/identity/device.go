package identity

import (
	"context"

	"time"

	"github.com/itsivali/lottery-platform/internal/identity/store"
	"github.com/itsivali/lottery-platform/internal/platform/crypto"
)

func (s *Service) fingerprintHash(fp string) []byte { return crypto.MAC(s.cfg.OTPKey, "device", fp) }

// registerDevice records the device and enforces the device-to-account cap. Mobile platforms must send a
// fingerprint; the web has none and is limited by IP and CAPTCHA instead.
func (s *Service) registerDevice(ctx context.Context, q *store.Queries, userID string, dev Device, verdict string, now time.Time) (*string, error) {
	switch dev.Platform {
	case "android", "ios":
		if dev.Fingerprint == "" {
			return nil, ErrDeviceRequired
		}
	case "web":
		if dev.Fingerprint == "" {
			return nil, nil
		}
	default:
		return nil, ErrDeviceRequired
	}
	hash := s.fingerprintHash(dev.Fingerprint)
	others, err := q.CountOtherAccountsOnDevice(ctx, store.CountOtherAccountsOnDeviceParams{FingerprintHash: hash, UserID: userID})
	if err != nil {
		return nil, err
	}
	if int(others) >= s.cfg.DeviceCap {
		return nil, ErrDeviceCap
	}
	id, err := q.UpsertDevice(ctx, store.UpsertDeviceParams{
		UserID: userID, FingerprintHash: hash, Platform: dev.Platform, AttestationVerdict: verdict, At: now,
	})
	if err != nil {
		return nil, err
	}
	return &id, nil
}
