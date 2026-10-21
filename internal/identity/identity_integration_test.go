//go:build integration

package identity_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/itsivali/lottery-platform/internal/audit"
	"github.com/itsivali/lottery-platform/internal/identity"
	"github.com/itsivali/lottery-platform/internal/platform/clock"
	"github.com/itsivali/lottery-platform/internal/platform/password"
	"github.com/itsivali/lottery-platform/internal/platform/testpg"
)

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
	svc  *identity.Service
	seen map[string]bool
}

var start = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

const goodPassword = "correct horse battery"

func setup(t *testing.T) *env {
	t.Helper()
	pool := testpg.Pool(t, "app_api")
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{pool: pool, clk: clock.NewManual(start), seen: map[string]bool{}}
	e.svc = identity.NewService(pool, e.clk, fakeOIDC{}, audit.NewWriter(), identity.Config{
		AuthKey: []byte("0123456789abcdef0123456789abcdef"), TokenKey: priv, Issuer: "lottery", Audience: "lottery-api",
		AccessTTL: 10 * time.Minute, RefreshTTL: 30 * 24 * time.Hour, DeviceCap: 2,
	})
	return e
}

var seq int

func (e *env) newMSISDN() string { seq++; return fmt.Sprintf("2547%08d", seq) }

// login signs the number up the first time it is seen and logs it in afterwards, with the standard test password.
func (e *env) login(t *testing.T, msisdn string, dev identity.Device) identity.Tokens {
	t.Helper()
	ctx := context.Background()
	var tok identity.Tokens
	var err error
	if e.seen[msisdn] {
		tok, err = e.svc.Login(ctx, msisdn, goodPassword, dev)
	} else {
		tok, err = e.svc.Signup(ctx, msisdn, goodPassword, dev)
		e.seen[msisdn] = true
	}
	if err != nil {
		t.Fatalf("sign in %s: %v", msisdn, err)
	}
	return tok
}

// verifyPhone marks a signed-up number as proved, as the payments module will after an STK confirmation.
func (e *env) verifyPhone(t *testing.T, userID, msisdn string) {
	t.Helper()
	if err := pgx.BeginFunc(context.Background(), e.pool, func(tx pgx.Tx) error { return e.svc.MarkPhoneVerified(context.Background(), tx, userID, msisdn) }); err != nil {
		t.Fatalf("MarkPhoneVerified: %v", err)
	}
}

var phone = identity.Device{Fingerprint: "fp-1", Platform: "android"}

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
	tok2 := e.login(t, e.newMSISDN(), identity.Device{Fingerprint: "fp-2", Platform: "ios"})
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
	tok = e.login(t, e.newMSISDN(), identity.Device{Fingerprint: "fp-9", Platform: "android"})
	e.clk.Advance(31 * 24 * time.Hour)
	if _, err := e.svc.Refresh(ctx, tok.Refresh, "fp-9"); !errors.Is(err, identity.ErrTokenInvalid) {
		t.Errorf("expired refresh token err = %v", err)
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
	if _, err := e.pool.Exec(ctx, `UPDATE identity.users SET kyc_status='verified', national_id_hash='\x01', msisdn_verified_at = now() WHERE id = $1`, p.UserID); err != nil {
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
		tok := e.login(t, e.newMSISDN(), identity.Device{Fingerprint: fp, Platform: "android"})
		p, _ := e.svc.Authenticate(ctx, tok.Access)
		if _, err := e.pool.Exec(ctx, `UPDATE identity.users SET kyc_status='verified', msisdn_verified_at = now(), national_id_hash=decode(md5($1::text),'hex') WHERE id = $2::uuid`, p.UserID, p.UserID); err != nil {
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

func TestEligibility_TopUpNeedsAVerifiedIDAndPlayAlsoNeedsAProvedPhone(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	m := e.newMSISDN()
	tok := e.login(t, m, phone)
	p, _ := e.svc.Authenticate(ctx, tok.Access)
	topUp := func() error {
		return pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error { return e.svc.AssertCanTopUp(ctx, tx, p.UserID) })
	}
	play := func() error {
		return pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error { return e.svc.AssertCanPlay(ctx, tx, p.UserID) })
	}
	for _, st := range []string{"none", "pending", "manual_review", "rejected"} {
		if _, err := e.pool.Exec(ctx, `UPDATE identity.users SET kyc_status = $1 WHERE id = $2`, st, p.UserID); err != nil {
			t.Fatal(err)
		}
		if err := topUp(); !errors.Is(err, identity.ErrKYCRequired) {
			t.Errorf("kyc %s: top-up err = %v, want ErrKYCRequired", st, err)
		}
	}
	if _, err := e.pool.Exec(ctx, `UPDATE identity.users SET kyc_status='verified', national_id_hash='\x02' WHERE id = $1`, p.UserID); err != nil {
		t.Fatal(err)
	}
	if err := topUp(); err != nil {
		t.Fatalf("a KYC-verified account must be able to start the top-up that proves its phone: %v", err)
	}
	if err := play(); !errors.Is(err, identity.ErrPhoneNotVerified) {
		t.Fatalf("play before the phone is proved: %v, want ErrPhoneNotVerified", err)
	}
	e.verifyPhone(t, p.UserID, m)
	if err := play(); err != nil {
		t.Fatalf("play after the phone is proved: %v", err)
	}
	for _, st := range []string{"suspended", "closed"} {
		if _, err := e.pool.Exec(ctx, `UPDATE identity.users SET status = $1 WHERE id = $2`, st, p.UserID); err != nil {
			t.Fatal(err)
		}
		if err := topUp(); !errors.Is(err, identity.ErrAccountSuspended) {
			t.Errorf("status %s: top-up err = %v", st, err)
		}
		if err := play(); !errors.Is(err, identity.ErrAccountSuspended) {
			t.Errorf("status %s: play err = %v", st, err)
		}
	}
}

// fakeAttestor passes tokens starting with "good", refuses "bad" and is down for "outage".
type fakeAttestor struct{}

func (fakeAttestor) Verify(_ context.Context, platform, token, nonce string) (bool, error) {
	switch {
	case strings.HasPrefix(token, "good"):
		return platform == "android" || platform == "ios", nil
	case token == "outage":
		return false, errors.New("attestation service down")
	}
	return false, nil
}

func TestAttestation_VerdictComesFromTheServerSideVerifierNeverFromTheClient(t *testing.T) {
	e := setup(t)
	e.svc.WithAttestor(fakeAttestor{})
	ctx := context.Background()
	verdictFor := func(token string) string {
		t.Helper()
		seq++
		m := e.newMSISDN()
		dev := identity.Device{Fingerprint: fmt.Sprintf("att-%d", seq), Platform: "android", AttestationToken: token}
		tok := e.login(t, m, dev)
		var v string
		if err := e.pool.QueryRow(ctx, `SELECT d.attestation_verdict FROM identity.devices d JOIN identity.sessions s ON s.device_id = d.id
			JOIN identity.users u ON u.id = s.user_id WHERE u.msisdn = $1`, m).Scan(&v); err != nil {
			t.Fatal(err)
		}
		_ = tok
		return v
	}
	if got := verdictFor("good-token"); got != "passed" {
		t.Errorf("valid token -> %q, want passed", got)
	}
	if got := verdictFor("bad-token"); got != "failed" {
		t.Errorf("rejected token -> %q, want failed", got)
	}
	if got := verdictFor(""); got != "none" {
		t.Errorf("no token -> %q, want none", got)
	}
	if got := verdictFor("outage"); got != "none" {
		t.Errorf("verifier outage -> %q, want none (unknown, not passed)", got)
	}
}

func TestAttestation_FailedDevicesCannotTransactAndStrictModeRequiresAPass(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name     string
		token    string
		platform string
		strict   bool
		wantErr  error
	}{
		{"passed device", "good", "android", true, nil},
		{"failed device, lenient", "bad", "android", false, identity.ErrAttestationFailed},
		{"failed device, strict", "bad", "android", true, identity.ErrAttestationFailed},
		{"unattested device, lenient", "", "android", false, nil},
		{"unattested device, strict", "", "android", true, identity.ErrAttestationFailed},
		{"web is not attestable", "", "web", true, nil},
	} {
		e := setup(t)
		e.svc.WithAttestor(fakeAttestor{})
		dev := identity.Device{Fingerprint: "fp-att", Platform: c.platform, AttestationToken: c.token}
		if c.platform == "web" {
			dev.Fingerprint = ""
		}
		tok := e.login(t, e.newMSISDN(), dev)
		p, _ := e.svc.Authenticate(ctx, tok.Access)
		err := pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error { return e.svc.AssertDeviceTrusted(ctx, tx, p.SessionID, c.strict) })
		if !errors.Is(err, c.wantErr) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.wantErr)
		}
	}
}

func TestSignup_CreatesAnUnverifiedAccountWithAHashedPassword(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	m := e.newMSISDN()
	tok := e.login(t, m, phone)
	p, err := e.svc.Authenticate(ctx, tok.Access)
	if err != nil {
		t.Fatalf("signup tokens do not work: %v", err)
	}
	var hash string
	var verified bool
	if err := e.pool.QueryRow(ctx, `SELECT password_hash, msisdn_verified_at IS NOT NULL FROM identity.users WHERE id = $1`, p.UserID).Scan(&hash, &verified); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "$argon2id$") || strings.Contains(hash, goodPassword) {
		t.Fatalf("stored password hash = %q", hash)
	}
	if verified {
		t.Fatal("a new account must not have a proved phone")
	}
	var audits int
	_ = e.pool.QueryRow(ctx, `SELECT count(*) FROM audit.audit_log WHERE action = 'user.created' AND entity_id = $1`, p.UserID).Scan(&audits)
	if audits != 1 {
		t.Fatalf("%d user.created audit rows", audits)
	}
}

func TestSignup_RefusesBadNumbersWeakPasswordsAndNumbersAlreadyProved(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	if _, err := e.svc.Signup(ctx, "12345", goodPassword, phone); !errors.Is(err, identity.ErrInvalidMSISDN) {
		t.Errorf("bad number: %v", err)
	}
	if _, err := e.svc.Signup(ctx, e.newMSISDN(), "password", phone); !errors.Is(err, password.ErrWeak) {
		t.Errorf("weak password: %v", err)
	}
	m := e.newMSISDN()
	tok := e.login(t, m, phone)
	p, _ := e.svc.Authenticate(ctx, tok.Access)
	e.verifyPhone(t, p.UserID, m)
	other := identity.Device{Fingerprint: "other-phone", Platform: "android"}
	if _, err := e.svc.Signup(ctx, m, "another strong one", other); !errors.Is(err, identity.ErrPhoneRegistered) {
		t.Errorf("signing up a proved number: %v, want ErrPhoneRegistered", err)
	}
}

func TestLogin_OnlyTheRightPasswordOpensTheAccountAndFailuresAreIndistinguishable(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	m := e.newMSISDN()
	e.login(t, m, phone)
	if _, err := e.svc.Login(ctx, m, goodPassword, phone); err != nil {
		t.Fatalf("right password: %v", err)
	}
	for name, c := range map[string]struct{ msisdn, pw string }{
		"wrong password": {m, "wrong password 1"}, "unknown number": {e.newMSISDN(), goodPassword}, "malformed number": {"nope", goodPassword}, "empty password": {m, ""},
	} {
		if _, err := e.svc.Login(ctx, c.msisdn, c.pw, phone); !errors.Is(err, identity.ErrInvalidCredentials) {
			t.Errorf("%s: err = %v, want ErrInvalidCredentials", name, err)
		}
	}
}

func TestLogin_SuspendedAccountsAreRefusedOnlyAfterTheRightPassword(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	m := e.newMSISDN()
	e.login(t, m, phone)
	if _, err := e.pool.Exec(ctx, `UPDATE identity.users SET status = 'suspended' WHERE msisdn = $1`, m); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Login(ctx, m, goodPassword, phone); !errors.Is(err, identity.ErrAccountSuspended) {
		t.Errorf("right password: %v, want ErrAccountSuspended", err)
	}
	if _, err := e.svc.Login(ctx, m, "wrong password 1", phone); !errors.Is(err, identity.ErrInvalidCredentials) {
		t.Errorf("wrong password must not reveal the suspension: %v", err)
	}
}

func TestClaims_SeveralPeopleMayClaimANumberButOnlyTheOneWhoProvesItKeepsIt(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	m := e.newMSISDN()
	squatter, err := e.svc.Signup(ctx, m, "squatters password 1", identity.Device{Fingerprint: "squatter-phone", Platform: "android"})
	if err != nil {
		t.Fatal(err)
	}
	owner, err := e.svc.Signup(ctx, m, "the real owners password", identity.Device{Fingerprint: "owner-phone", Platform: "android"})
	if err != nil {
		t.Fatalf("a squatter's unverified claim must not block the real owner: %v", err)
	}
	// each claimant gets into their own account
	if _, err := e.svc.Login(ctx, m, "squatters password 1", phone); err != nil {
		t.Errorf("squatter's own login: %v", err)
	}
	if _, err := e.svc.Login(ctx, m, "the real owners password", phone); err != nil {
		t.Errorf("owner's login: %v", err)
	}
	ownerP, _ := e.svc.Authenticate(ctx, owner.Access)
	squatterP, _ := e.svc.Authenticate(ctx, squatter.Access)
	e.verifyPhone(t, ownerP.UserID, m) // the owner passes the M-Pesa proof first
	if err := pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error { return e.svc.MarkPhoneVerified(ctx, tx, squatterP.UserID, m) }); !errors.Is(err, identity.ErrPhoneTaken) {
		t.Errorf("squatter verifying a proved number: %v, want ErrPhoneTaken", err)
	}
	if _, err := e.svc.Login(ctx, m, "squatters password 1", phone); !errors.Is(err, identity.ErrInvalidCredentials) {
		t.Errorf("squatter can still log in after the owner proved the number: %v", err)
	}
	if _, err := e.svc.Login(ctx, m, "the real owners password", phone); err != nil {
		t.Errorf("owner locked out: %v", err)
	}
}

func TestMarkPhoneVerified_RulesAndIdempotence(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	m := e.newMSISDN()
	tok := e.login(t, m, phone)
	p, _ := e.svc.Authenticate(ctx, tok.Access)
	mark := func(user, msisdn string) error {
		return pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error { return e.svc.MarkPhoneVerified(ctx, tx, user, msisdn) })
	}
	if err := mark(p.UserID, e.newMSISDN()); !errors.Is(err, identity.ErrPhoneMismatch) {
		t.Errorf("payer number differs from the claimed number: %v, want ErrPhoneMismatch", err)
	}
	if err := mark(p.UserID, m); err != nil {
		t.Fatal(err)
	}
	var first time.Time
	_ = e.pool.QueryRow(ctx, `SELECT msisdn_verified_at FROM identity.users WHERE id = $1`, p.UserID).Scan(&first)
	e.clk.Advance(time.Hour)
	if err := mark(p.UserID, m); err != nil {
		t.Errorf("repeating the proof must be harmless: %v", err)
	}
	var second time.Time
	_ = e.pool.QueryRow(ctx, `SELECT msisdn_verified_at FROM identity.users WHERE id = $1`, p.UserID).Scan(&second)
	if !first.Equal(second) {
		t.Errorf("verification time moved: %v -> %v", first, second)
	}
	if err := mark("00000000-0000-7000-8000-000000000999", m); err == nil {
		t.Error("an unknown account was verified")
	}
}

func TestChangePassword(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	m := e.newMSISDN()
	current := e.login(t, m, phone)
	other, err := e.svc.Login(ctx, m, goodPassword, identity.Device{Fingerprint: "second-device", Platform: "android"})
	if err != nil {
		t.Fatal(err)
	}
	p, _ := e.svc.Authenticate(ctx, current.Access)
	if err := e.svc.ChangePassword(ctx, p.UserID, p.SessionID, "not my password 1", "a brand new passphrase"); !errors.Is(err, identity.ErrInvalidCredentials) {
		t.Errorf("wrong current password: %v", err)
	}
	if err := e.svc.ChangePassword(ctx, p.UserID, p.SessionID, goodPassword, "password"); !errors.Is(err, password.ErrWeak) {
		t.Errorf("weak new password: %v", err)
	}
	if err := e.svc.ChangePassword(ctx, p.UserID, p.SessionID, goodPassword, "a brand new passphrase"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Login(ctx, m, goodPassword, phone); !errors.Is(err, identity.ErrInvalidCredentials) {
		t.Errorf("old password still works: %v", err)
	}
	if _, err := e.svc.Login(ctx, m, "a brand new passphrase", phone); err != nil {
		t.Errorf("new password: %v", err)
	}
	if _, err := e.svc.Authenticate(ctx, current.Access); err != nil {
		t.Errorf("the session that changed the password must survive: %v", err)
	}
	if _, err := e.svc.Authenticate(ctx, other.Access); !errors.Is(err, identity.ErrTokenInvalid) {
		t.Errorf("another device's session survived a password change: %v", err)
	}
	if _, err := e.svc.Refresh(ctx, other.Refresh, "second-device"); !errors.Is(err, identity.ErrTokenInvalid) {
		t.Errorf("another device's refresh token survived a password change: %v", err)
	}
}

func TestDevices_ThirdAccountOnTheSameDeviceIsRefused(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	dev := identity.Device{Fingerprint: "shared-phone", Platform: "android"}
	a, b := e.newMSISDN(), e.newMSISDN()
	e.login(t, a, dev)
	e.login(t, b, dev)
	e.login(t, a, dev) // an existing link is allowed
	if _, err := e.svc.Signup(ctx, e.newMSISDN(), goodPassword, dev); !errors.Is(err, identity.ErrDeviceCap) {
		t.Fatalf("third account err = %v, want ErrDeviceCap", err)
	}
	// the refused signup must not leave an account behind
	var n int
	_ = e.pool.QueryRow(ctx, `SELECT count(*) FROM identity.users`).Scan(&n)
	if n != 2 {
		t.Fatalf("%d accounts exist, want 2", n)
	}
}

func TestDevices_MobileSignupWithoutAFingerprintIsRefused(t *testing.T) {
	e := setup(t)
	if _, err := e.svc.Signup(context.Background(), e.newMSISDN(), goodPassword, identity.Device{Platform: "android"}); !errors.Is(err, identity.ErrDeviceRequired) {
		t.Fatalf("err = %v, want ErrDeviceRequired", err)
	}
}
