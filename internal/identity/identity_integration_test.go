//go:build integration

package identity_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/itsivali/lottery-platform/internal/audit"
	"github.com/itsivali/lottery-platform/internal/identity"
	"github.com/itsivali/lottery-platform/internal/platform/clock"
	"github.com/itsivali/lottery-platform/internal/platform/testpg"
)

type fakeSMS struct {
	mu    sync.Mutex
	codes map[string]string
	sent  int
	fail  error
}

var digits = regexp.MustCompile(`\b\d{6}\b`)

func (f *fakeSMS) Send(_ context.Context, msisdn, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	f.sent++
	if f.codes == nil {
		f.codes = map[string]string{}
	}
	f.codes[msisdn] = digits.FindString(text)
	return nil
}

func (f *fakeSMS) code(msisdn string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.codes[msisdn]
}

// fakeOIDC accepts tokens of the form "ok:<subject>" and refuses everything else.
type fakeOIDC struct{}

func (fakeOIDC) VerifyIDToken(_ context.Context, provider, token, nonce string) (string, error) {
	if provider != "google" && provider != "apple" {
		return "", errors.New("unknown provider")
	}
	if nonce != "n1" {
		return "", errors.New("bad nonce")
	}
	sub, ok := strings.CutPrefix(token, "ok:")
	if !ok {
		return "", errors.New("bad token")
	}
	return sub, nil
}

type env struct {
	pool *pgxpool.Pool
	clk  *clock.Manual
	sms  *fakeSMS
	svc  *identity.Service
}

var start = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

func setup(t *testing.T) *env {
	t.Helper()
	pool := testpg.Pool(t, "app_api")
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{pool: pool, clk: clock.NewManual(start), sms: &fakeSMS{}}
	e.svc = identity.NewService(pool, e.clk, e.sms, fakeOIDC{}, audit.NewWriter(), identity.Config{
		OTPKey: []byte("0123456789abcdef0123456789abcdef"), TokenKey: priv, Issuer: "lottery", Audience: "lottery-api",
		AccessTTL: 10 * time.Minute, RefreshTTL: 30 * 24 * time.Hour, OTPTTL: 5 * time.Minute, OTPCooldown: 60 * time.Second, DeviceCap: 2,
	})
	return e
}

var seq int

func (e *env) newMSISDN() string { seq++; return fmt.Sprintf("2547%08d", seq) }

func (e *env) login(t *testing.T, msisdn string, dev identity.Device) identity.Tokens {
	t.Helper()
	ctx := context.Background()
	if err := e.svc.RequestOTP(ctx, msisdn, identity.PurposeLogin, "203.0.113.9"); err != nil {
		t.Fatalf("RequestOTP: %v", err)
	}
	tok, err := e.svc.VerifyOTP(ctx, msisdn, e.sms.code(msisdn), dev)
	if err != nil {
		t.Fatalf("VerifyOTP: %v", err)
	}
	return tok
}

var phone = identity.Device{Fingerprint: "fp-1", Platform: "android", Attestation: "passed"}

func TestOTP_RequestSendsASixDigitCodeAndStoresOnlyItsMAC(t *testing.T) {
	e := setup(t)
	m := e.newMSISDN()
	if err := e.svc.RequestOTP(context.Background(), m, identity.PurposeLogin, "203.0.113.9"); err != nil {
		t.Fatal(err)
	}
	code := e.sms.code(m)
	if len(code) != 6 {
		t.Fatalf("code = %q", code)
	}
	var hmacLen int
	var plain bool
	if err := e.pool.QueryRow(context.Background(), `SELECT length(code_hmac), position($1 in encode(code_hmac,'escape')) > 0 FROM identity.phone_otps`, code).Scan(&hmacLen, &plain); err != nil {
		t.Fatal(err)
	}
	if hmacLen != 32 || plain {
		t.Fatalf("hmac length %d, plaintext visible %v", hmacLen, plain)
	}
}

func TestOTP_RejectsMalformedNumbers(t *testing.T) {
	e := setup(t)
	if err := e.svc.RequestOTP(context.Background(), "12345", identity.PurposeLogin, ""); !errors.Is(err, identity.ErrInvalidMSISDN) {
		t.Fatalf("err = %v", err)
	}
	if e.sms.sent != 0 {
		t.Fatal("SMS sent for an invalid number")
	}
}

func TestOTP_ResendIsBlockedUntilTheCooldownPasses(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	m := e.newMSISDN()
	if err := e.svc.RequestOTP(ctx, m, identity.PurposeLogin, ""); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(30 * time.Second)
	var cd *identity.CooldownError
	if err := e.svc.RequestOTP(ctx, m, identity.PurposeLogin, ""); !errors.As(err, &cd) || cd.RetryAfter != 30*time.Second {
		t.Fatalf("err = %v, want cooldown of 30s", err)
	}
	e.clk.Advance(31 * time.Second)
	if err := e.svc.RequestOTP(ctx, m, identity.PurposeLogin, ""); err != nil {
		t.Fatalf("after cooldown: %v", err)
	}
}

func TestOTP_SMSFailureIsReported(t *testing.T) {
	e := setup(t)
	e.sms.fail = errors.New("gateway down")
	if err := e.svc.RequestOTP(context.Background(), e.newMSISDN(), identity.PurposeLogin, ""); err == nil {
		t.Fatal("SMS failure swallowed")
	}
}

func TestOTP_VerifyCreatesTheAccountOnFirstLoginAndReusesItAfter(t *testing.T) {
	e := setup(t)
	m := e.newMSISDN()
	first := e.login(t, m, phone)
	e.clk.Advance(2 * time.Minute)
	second := e.login(t, m, phone)
	a, err := e.svc.Authenticate(context.Background(), first.Access)
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.svc.Authenticate(context.Background(), second.Access)
	if err != nil {
		t.Fatal(err)
	}
	if a.UserID != b.UserID || a.SessionID == b.SessionID {
		t.Fatalf("same user expected with distinct sessions: %+v %+v", a, b)
	}
}

func TestOTP_CodeIsSingleUse(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	m := e.newMSISDN()
	e.login(t, m, phone)
	if _, err := e.svc.VerifyOTP(ctx, m, e.sms.code(m), phone); !errors.Is(err, identity.ErrOTPInvalid) {
		t.Fatalf("reuse err = %v", err)
	}
}

func TestOTP_WrongCodeIsRefusedAndFiveFailuresBurnTheCode(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	m := e.newMSISDN()
	if err := e.svc.RequestOTP(ctx, m, identity.PurposeLogin, ""); err != nil {
		t.Fatal(err)
	}
	right := e.sms.code(m)
	wrong := "000000"
	if right == wrong {
		wrong = "000001"
	}
	for i := range 5 {
		if _, err := e.svc.VerifyOTP(ctx, m, wrong, phone); !errors.Is(err, identity.ErrOTPInvalid) {
			t.Fatalf("attempt %d err = %v", i, err)
		}
	}
	if _, err := e.svc.VerifyOTP(ctx, m, right, phone); !errors.Is(err, identity.ErrOTPInvalid) {
		t.Fatalf("correct code accepted after 5 failures: %v", err)
	}
}

func TestOTP_ExpiresAfterFiveMinutes(t *testing.T) {
	e := setup(t)
	m := e.newMSISDN()
	if err := e.svc.RequestOTP(context.Background(), m, identity.PurposeLogin, ""); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(5*time.Minute + time.Second)
	if _, err := e.svc.VerifyOTP(context.Background(), m, e.sms.code(m), phone); !errors.Is(err, identity.ErrOTPInvalid) {
		t.Fatalf("expired code accepted: %v", err)
	}
}

func TestOTP_CodeForAnotherNumberIsRefused(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	a, b := e.newMSISDN(), e.newMSISDN()
	_ = e.svc.RequestOTP(ctx, a, identity.PurposeLogin, "")
	_ = e.svc.RequestOTP(ctx, b, identity.PurposeLogin, "")
	if _, err := e.svc.VerifyOTP(ctx, b, e.sms.code(a), phone); !errors.Is(err, identity.ErrOTPInvalid) && e.sms.code(a) != e.sms.code(b) {
		t.Fatalf("a's code opened b's account: %v", err)
	}
}

func TestAccessToken_AuthenticatesUntilItExpiresOrTheSessionEnds(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	tok := e.login(t, e.newMSISDN(), phone)
	p, err := e.svc.Authenticate(ctx, tok.Access)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Authenticate(ctx, tok.Access+"x"); !errors.Is(err, identity.ErrTokenInvalid) {
		t.Errorf("tampered token err = %v", err)
	}
	if _, err := e.svc.Authenticate(ctx, ""); !errors.Is(err, identity.ErrTokenInvalid) {
		t.Errorf("empty token err = %v", err)
	}
	if err := e.svc.Logout(ctx, p.SessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Authenticate(ctx, tok.Access); !errors.Is(err, identity.ErrTokenInvalid) {
		t.Errorf("token for a logged-out session accepted: %v", err)
	}
	tok2 := e.login(t, e.newMSISDN(), identity.Device{Fingerprint: "fp-2", Platform: "ios", Attestation: "passed"})
	e.clk.Advance(10*time.Minute + time.Second)
	if _, err := e.svc.Authenticate(ctx, tok2.Access); !errors.Is(err, identity.ErrTokenInvalid) {
		t.Errorf("expired token accepted: %v", err)
	}
}

func TestAccessToken_SignedByAnotherKeyIsRefused(t *testing.T) {
	e := setup(t)
	other := setup(t)
	tok := other.login(t, other.newMSISDN(), phone)
	if _, err := e.svc.Authenticate(context.Background(), tok.Access); !errors.Is(err, identity.ErrTokenInvalid) {
		t.Fatalf("foreign token accepted: %v", err)
	}
}

func TestRefresh_RotatesAndRefusesReuseByRevokingTheWholeChain(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	t0 := e.login(t, e.newMSISDN(), phone)
	t1, err := e.svc.Refresh(ctx, t0.Refresh, phone.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if t1.Refresh == t0.Refresh {
		t.Fatal("refresh token not rotated")
	}
	t2, err := e.svc.Refresh(ctx, t1.Refresh, phone.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	// an attacker replays the first, already-rotated token
	if _, err := e.svc.Refresh(ctx, t0.Refresh, phone.Fingerprint); !errors.Is(err, identity.ErrTokenReuse) {
		t.Fatalf("reuse err = %v, want ErrTokenReuse", err)
	}
	// the legitimate newest token and its access token are now dead as well
	if _, err := e.svc.Refresh(ctx, t2.Refresh, phone.Fingerprint); !errors.Is(err, identity.ErrTokenInvalid) {
		t.Fatalf("chain survived reuse: %v", err)
	}
	if _, err := e.svc.Authenticate(ctx, t2.Access); !errors.Is(err, identity.ErrTokenInvalid) {
		t.Fatalf("access token survived reuse: %v", err)
	}
	var n int
	_ = e.pool.QueryRow(ctx, `SELECT count(*) FROM audit.audit_log WHERE action = 'auth.refresh_reuse'`).Scan(&n)
	if n != 1 {
		t.Fatalf("%d reuse audit rows, want 1", n)
	}
}

func TestRefresh_ConcurrentUseOfOneTokenSucceedsOnce(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	t0 := e.login(t, e.newMSISDN(), phone)
	var wg sync.WaitGroup
	results := make(chan error, 10)
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := e.svc.Refresh(ctx, t0.Refresh, phone.Fingerprint)
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	ok := 0
	for err := range results {
		if err == nil {
			ok++
		}
	}
	if ok != 1 {
		t.Fatalf("%d refreshes succeeded, want exactly 1", ok)
	}
}

func TestRefresh_UnknownExpiredAndWrongDeviceTokensAreRefused(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	if _, err := e.svc.Refresh(ctx, "not-a-token", phone.Fingerprint); !errors.Is(err, identity.ErrTokenInvalid) {
		t.Errorf("unknown token err = %v", err)
	}
	tok := e.login(t, e.newMSISDN(), phone)
	if _, err := e.svc.Refresh(ctx, tok.Refresh, "someone-elses-phone"); !errors.Is(err, identity.ErrTokenInvalid) {
		t.Errorf("wrong device err = %v", err)
	}
	// the stolen-token attempt killed the session
	if _, err := e.svc.Refresh(ctx, tok.Refresh, phone.Fingerprint); !errors.Is(err, identity.ErrTokenInvalid) {
		t.Errorf("session survived a wrong-device refresh: %v", err)
	}
	tok = e.login(t, e.newMSISDN(), identity.Device{Fingerprint: "fp-9", Platform: "android", Attestation: "passed"})
	e.clk.Advance(31 * 24 * time.Hour)
	if _, err := e.svc.Refresh(ctx, tok.Refresh, "fp-9"); !errors.Is(err, identity.ErrTokenInvalid) {
		t.Errorf("expired refresh token err = %v", err)
	}
}

func TestDevices_ThirdAccountOnTheSameDeviceIsRefused(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	dev := identity.Device{Fingerprint: "shared-phone", Platform: "android", Attestation: "passed"}
	a, b, c := e.newMSISDN(), e.newMSISDN(), e.newMSISDN()
	e.login(t, a, dev)
	e.login(t, b, dev)
	e.clk.Advance(2 * time.Minute)
	e.login(t, a, dev) // existing link: allowed
	if err := e.svc.RequestOTP(ctx, c, identity.PurposeLogin, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.VerifyOTP(ctx, c, e.sms.code(c), dev); !errors.Is(err, identity.ErrDeviceCap) {
		t.Fatalf("third account err = %v, want ErrDeviceCap", err)
	}
}

func TestDevices_MobileLoginWithoutAFingerprintIsRefused(t *testing.T) {
	e := setup(t)
	m := e.newMSISDN()
	_ = e.svc.RequestOTP(context.Background(), m, identity.PurposeLogin, "")
	_, err := e.svc.VerifyOTP(context.Background(), m, e.sms.code(m), identity.Device{Platform: "android"})
	if !errors.Is(err, identity.ErrDeviceRequired) {
		t.Fatalf("err = %v, want ErrDeviceRequired", err)
	}
}

func TestSuspendedAccountsCannotLogIn(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	m := e.newMSISDN()
	e.login(t, m, phone)
	if _, err := e.pool.Exec(ctx, `UPDATE identity.users SET status = 'suspended' WHERE msisdn = $1`, m); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(2 * time.Minute)
	_ = e.svc.RequestOTP(ctx, m, identity.PurposeLogin, "")
	if _, err := e.svc.VerifyOTP(ctx, m, e.sms.code(m), phone); !errors.Is(err, identity.ErrAccountSuspended) {
		t.Fatalf("err = %v, want ErrAccountSuspended", err)
	}
}

func TestSSO_LinkingRequiresAVerifiedKYCAndSignInRequiresALink(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	m := e.newMSISDN()
	tok := e.login(t, m, phone)
	p, _ := e.svc.Authenticate(ctx, tok.Access)

	if _, err := e.svc.SignInSSO(ctx, "google", "ok:g-123", "n1", phone); !errors.Is(err, identity.ErrSSONotLinked) {
		t.Fatalf("unlinked sign-in err = %v", err)
	}
	if err := e.svc.LinkSSO(ctx, p.UserID, "google", "ok:g-123", "n1"); !errors.Is(err, identity.ErrKYCRequired) {
		t.Fatalf("link before KYC err = %v", err)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE identity.users SET kyc_status='verified', national_id_hash='\x01' WHERE id = $1`, p.UserID); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.LinkSSO(ctx, p.UserID, "google", "ok:g-123", "n1"); err != nil {
		t.Fatalf("link: %v", err)
	}
	signed, err := e.svc.SignInSSO(ctx, "google", "ok:g-123", "n1", phone)
	if err != nil {
		t.Fatalf("SSO sign-in: %v", err)
	}
	if q, err := e.svc.Authenticate(ctx, signed.Access); err != nil || q.UserID != p.UserID {
		t.Fatalf("SSO session belongs to %v, %v", q, err)
	}
}

func TestSSO_SubjectCannotBeLinkedToTwoAccountsAndBadTokensAreRefused(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	mk := func(fp string) string {
		tok := e.login(t, e.newMSISDN(), identity.Device{Fingerprint: fp, Platform: "android", Attestation: "passed"})
		p, _ := e.svc.Authenticate(ctx, tok.Access)
		if _, err := e.pool.Exec(ctx, `UPDATE identity.users SET kyc_status='verified', national_id_hash=decode(md5($1::text),'hex') WHERE id = $2::uuid`, p.UserID, p.UserID); err != nil {
			t.Fatal(err)
		}
		return p.UserID
	}
	u1, u2 := mk("fp-a"), mk("fp-b")
	if err := e.svc.LinkSSO(ctx, u1, "apple", "ok:sub-1", "n1"); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.LinkSSO(ctx, u2, "apple", "ok:sub-1", "n1"); !errors.Is(err, identity.ErrSSOAlreadyLinked) {
		t.Fatalf("double link err = %v", err)
	}
	if err := e.svc.LinkSSO(ctx, u2, "apple", "forged", "n1"); !errors.Is(err, identity.ErrTokenInvalid) {
		t.Fatalf("forged id token err = %v", err)
	}
	if _, err := e.svc.SignInSSO(ctx, "apple", "ok:sub-1", "wrong-nonce", phone); !errors.Is(err, identity.ErrTokenInvalid) {
		t.Fatalf("wrong nonce err = %v", err)
	}
}
