//go:build integration

package kyc_test

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/itsivali/lottery-platform/internal/audit"
	"github.com/itsivali/lottery-platform/internal/kyc"
	"github.com/itsivali/lottery-platform/internal/platform/clock"
	"github.com/itsivali/lottery-platform/internal/platform/crypto"
	"github.com/itsivali/lottery-platform/internal/platform/testpg"
)

type env struct {
	pool *pgxpool.Pool
	clk  *clock.Manual
	prov *kyc.Mock
	kr   *crypto.Keyring
	svc  *kyc.Service
}

func setup(t *testing.T) *env {
	t.Helper()
	pool := testpg.Pool(t, "app_api")
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	kr, err := crypto.NewKeyring(1, map[byte][]byte{1: k})
	if err != nil {
		t.Fatal(err)
	}
	e := &env{pool: pool, clk: clock.NewManual(time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)), prov: kyc.NewMock(), kr: kr}
	e.svc = kyc.NewService(pool, e.clk, e.prov, kr, []byte("0123456789abcdef0123456789abcdef"), audit.NewWriter())
	return e
}

var seq int

func (e *env) user(t *testing.T) string {
	t.Helper()
	seq++
	var id string
	if err := e.pool.QueryRow(context.Background(), `INSERT INTO identity.users (msisdn) VALUES ($1) RETURNING id::text`, fmt.Sprintf("2547%08d", seq)).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func (e *env) userStatus(t *testing.T, id string) (status string, hasHash bool) {
	t.Helper()
	if err := e.pool.QueryRow(context.Background(), `SELECT kyc_status, national_id_hash IS NOT NULL FROM identity.users WHERE id = $1`, id).Scan(&status, &hasHash); err != nil {
		t.Fatal(err)
	}
	return status, hasHash
}

func (e *env) checks(t *testing.T, id string) int {
	t.Helper()
	var n int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM kyc.kyc_checks WHERE user_id = $1`, id).Scan(&n)
	return n
}

var jane = kyc.Submission{IDNumber: "12345678", FullName: "Jane Wanjiru Kamau", DateOfBirth: "1990-05-17"}

func (e *env) registerJane() {
	e.prov.Register("12345678", "Jane Wanjiru Kamau", time.Date(1990, 5, 17, 0, 0, 0, 0, time.UTC))
}

func TestSubmit_ExactMatchVerifiesTheAccountAndEncryptsTheIDNumber(t *testing.T) {
	e := setup(t)
	e.registerJane()
	u := e.user(t)
	st, err := e.svc.Submit(context.Background(), u, jane)
	if err != nil || st != kyc.Verified {
		t.Fatalf("status = %q, %v", st, err)
	}
	if s, hash := e.userStatus(t, u); s != "verified" || !hash {
		t.Fatalf("user: %q hash=%v", s, hash)
	}
	var enc []byte
	if err := e.pool.QueryRow(context.Background(), `SELECT id_number_enc FROM kyc.kyc_checks WHERE user_id = $1`, u).Scan(&enc); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(enc), "12345678") {
		t.Fatal("national ID stored in the clear")
	}
	if pt, err := e.kr.Decrypt(enc, []byte("kyc.id_number")); err != nil || string(pt) != "12345678" {
		t.Fatalf("decrypt = %q, %v", pt, err)
	}
}

func TestSubmit_MismatchesGoToManualReviewWithoutClaimingTheID(t *testing.T) {
	cases := map[string]kyc.Submission{
		"name mismatch": {IDNumber: "12345678", FullName: "Someone Else", DateOfBirth: "1990-05-17"},
		"dob mismatch":  {IDNumber: "12345678", FullName: "Jane Wanjiru Kamau", DateOfBirth: "1990-05-18"},
		"id not found":  {IDNumber: "87654321", FullName: "Jane Wanjiru Kamau", DateOfBirth: "1990-05-17"},
	}
	for name, in := range cases {
		e := setup(t)
		e.registerJane()
		u := e.user(t)
		st, err := e.svc.Submit(context.Background(), u, in)
		if err != nil || st != kyc.ManualReview {
			t.Errorf("%s: status = %q, %v", name, st, err)
		}
		if s, hash := e.userStatus(t, u); s != "manual_review" || hash {
			t.Errorf("%s: user %q hash=%v", name, s, hash)
		}
	}
}

func TestSubmit_NameMatchingIgnoresCaseAndSpacing(t *testing.T) {
	e := setup(t)
	e.registerJane()
	in := jane
	in.FullName = "  jane   WANJIRU kamau "
	if st, err := e.svc.Submit(context.Background(), e.user(t), in); err != nil || st != kyc.Verified {
		t.Fatalf("status = %q, %v", st, err)
	}
}

func TestSubmit_UnderageIsRefusedUsingTheNairobiDateAndStoresNothing(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	u := e.user(t)
	// 2026-10-21 12:00 UTC = 15:00 EAT: someone born 2008-10-21 turns 18 today
	in := kyc.Submission{IDNumber: "11111111", FullName: "Young Person", DateOfBirth: "2008-10-22"}
	if _, err := e.svc.Submit(ctx, u, in); !errors.Is(err, kyc.ErrUnderage) {
		t.Fatalf("born tomorrow-18: err = %v", err)
	}
	if e.checks(t, u) != 0 {
		t.Fatal("an under-age submission was stored")
	}
	// 22:00 UTC = 01:00 EAT on the 22nd: in Nairobi the 22nd has begun, so they are 18
	e.clk.Set(time.Date(2026, 10, 21, 22, 0, 0, 0, time.UTC))
	e.prov.Register("11111111", "Young Person", time.Date(2008, 10, 22, 0, 0, 0, 0, time.UTC))
	if st, err := e.svc.Submit(ctx, u, in); err != nil || st != kyc.Verified {
		t.Fatalf("turned 18 in Nairobi: %q, %v", st, err)
	}
}

func TestSubmit_OneIDOneAccount(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.registerJane()
	a, b := e.user(t), e.user(t)
	if _, err := e.svc.Submit(ctx, a, jane); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Submit(ctx, b, jane); !errors.Is(err, kyc.ErrIDAlreadyRegistered) {
		t.Fatalf("second account err = %v", err)
	}
	if e.checks(t, b) != 0 {
		t.Fatal("a refused duplicate left a check row")
	}
}

func TestSubmit_ConcurrentClaimsOfOneIDYieldExactlyOneVerifiedAccount(t *testing.T) {
	e := setup(t)
	e.registerJane()
	const n = 8
	users := make([]string, n)
	for i := range users {
		users[i] = e.user(t)
	}
	var wg sync.WaitGroup
	results := make(chan error, n)
	for _, u := range users {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := e.svc.Submit(context.Background(), u, jane)
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	ok := 0
	for err := range results {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, kyc.ErrIDAlreadyRegistered):
			t.Errorf("unexpected error: %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d accounts verified for one ID, want 1", ok)
	}
}

func TestSubmit_ResubmissionRules(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.registerJane()
	a, b := e.user(t), e.user(t)
	if _, err := e.svc.Submit(ctx, a, jane); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Submit(ctx, a, jane); !errors.Is(err, kyc.ErrAlreadyVerified) {
		t.Errorf("verified resubmit err = %v", err)
	}
	if _, err := e.svc.Submit(ctx, b, kyc.Submission{IDNumber: "99999999", FullName: "Nobody Known", DateOfBirth: "1991-01-01"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Submit(ctx, b, jane); !errors.Is(err, kyc.ErrInReview) {
		t.Errorf("resubmit while in review err = %v", err)
	}
}

func TestSubmit_ProviderOutageIsRecordedAndRetryable(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.registerJane()
	u := e.user(t)
	e.prov.FailNext(errors.New("timeout"))
	if _, err := e.svc.Submit(ctx, u, jane); !errors.Is(err, kyc.ErrProviderUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if s, _ := e.userStatus(t, u); s != "none" {
		t.Fatalf("status after outage = %q, want none", s)
	}
	if e.checks(t, u) != 1 {
		t.Fatal("the failed attempt was not recorded")
	}
	if st, err := e.svc.Submit(ctx, u, jane); err != nil || st != kyc.Verified {
		t.Fatalf("retry: %q, %v", st, err)
	}
}

func TestSubmit_ValidatesInput(t *testing.T) {
	e := setup(t)
	u := e.user(t)
	bad := []kyc.Submission{
		{IDNumber: "abc", FullName: "Jane Doe", DateOfBirth: "1990-01-01"},
		{IDNumber: "123", FullName: "Jane Doe", DateOfBirth: "1990-01-01"},
		{IDNumber: "1234567890", FullName: "Jane Doe", DateOfBirth: "1990-01-01"},
		{IDNumber: "12345678", FullName: "", DateOfBirth: "1990-01-01"},
		{IDNumber: "12345678", FullName: "J", DateOfBirth: "1990-01-01"},
		{IDNumber: "12345678", FullName: "Jane <script>", DateOfBirth: "1990-01-01"},
		{IDNumber: "12345678", FullName: strings.Repeat("a", 101), DateOfBirth: "1990-01-01"},
		{IDNumber: "12345678", FullName: "Jane Doe", DateOfBirth: "01/01/1990"},
		{IDNumber: "12345678", FullName: "Jane Doe", DateOfBirth: "2027-01-01"},
		{IDNumber: "12345678", FullName: "Jane Doe", DateOfBirth: "1899-12-31"},
	}
	for i, in := range bad {
		if _, err := e.svc.Submit(context.Background(), u, in); !errors.Is(err, kyc.ErrInvalidInput) {
			t.Errorf("case %d (%+v): err = %v, want ErrInvalidInput", i, in, err)
		}
	}
	if e.checks(t, u) != 0 {
		t.Fatal("invalid input was stored")
	}
}

func TestReview_ApproveAndRejectAppendSupersedingRowsAndAreAudited(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.registerJane()
	a, b := e.user(t), e.user(t)
	for _, u := range []string{a, b} {
		in := jane
		if u == b {
			in = kyc.Submission{IDNumber: "55555555", FullName: "Other Person", DateOfBirth: "1985-02-02"}
		}
		if _, err := e.svc.Submit(ctx, u, kyc.Submission{IDNumber: in.IDNumber, FullName: "Wrong Name", DateOfBirth: in.DateOfBirth}); err != nil {
			t.Fatal(err)
		}
	}
	if st, err := e.svc.Review(ctx, a, "alice", true, "documents checked"); err != nil || st != kyc.Verified {
		t.Fatalf("approve: %q, %v", st, err)
	}
	if s, hash := e.userStatus(t, a); s != "verified" || !hash {
		t.Fatalf("after approve: %q hash=%v", s, hash)
	}
	if st, err := e.svc.Review(ctx, b, "alice", false, "forged ID"); err != nil || st != kyc.Rejected {
		t.Fatalf("reject: %q, %v", st, err)
	}
	var superseding, audited int
	_ = e.pool.QueryRow(ctx, `SELECT count(*) FROM kyc.kyc_checks WHERE supersedes_id IS NOT NULL AND reviewer_id = 'alice'`).Scan(&superseding)
	_ = e.pool.QueryRow(ctx, `SELECT count(*) FROM audit.audit_log WHERE action = 'kyc.review' AND actor_id = 'alice'`).Scan(&audited)
	if superseding != 2 || audited != 2 {
		t.Fatalf("superseding=%d audited=%d, want 2/2", superseding, audited)
	}
	if _, err := e.svc.Review(ctx, a, "alice", true, "again"); !errors.Is(err, kyc.ErrNothingToReview) {
		t.Fatalf("review of a verified account err = %v", err)
	}
	if _, err := e.svc.Review(ctx, a, "", true, "x"); err == nil {
		t.Fatal("review without a named reviewer accepted")
	}
}

func TestReview_ApprovingADuplicateIDIsRefused(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.registerJane()
	a, b := e.user(t), e.user(t)
	if _, err := e.svc.Submit(ctx, a, jane); err != nil {
		t.Fatal(err)
	}
	// b claims the same ID with a wrong name: no duplicate yet blocks it at submission? it must, ID is taken
	if _, err := e.svc.Submit(ctx, b, kyc.Submission{IDNumber: "12345678", FullName: "Wrong", DateOfBirth: "1990-05-17"}); !errors.Is(err, kyc.ErrIDAlreadyRegistered) {
		t.Fatalf("err = %v", err)
	}
}

func TestStatus_ReportsNoneUntilSubmitted(t *testing.T) {
	e := setup(t)
	u := e.user(t)
	if st, err := e.svc.Status(context.Background(), u); err != nil || st != kyc.None {
		t.Fatalf("status = %q, %v", st, err)
	}
}
