//go:build integration

package payments_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/itsivali/lottery-platform/internal/audit"
	"github.com/itsivali/lottery-platform/internal/identity"
	"github.com/itsivali/lottery-platform/internal/ledger"
	"github.com/itsivali/lottery-platform/internal/payments"
	"github.com/itsivali/lottery-platform/internal/payments/daraja"
	"github.com/itsivali/lottery-platform/internal/payments/darajamock"
	"github.com/itsivali/lottery-platform/internal/platform/clock"
	"github.com/itsivali/lottery-platform/internal/platform/testpg"
	"github.com/itsivali/lottery-platform/internal/rg"
	"github.com/itsivali/lottery-platform/internal/wallet"
)

// queue records the jobs the service enqueues so a test can run them when it chooses.
type queue struct {
	mu   sync.Mutex
	jobs []job
}

type job struct{ kind, paymentID string }

func (q *queue) EnqueuePush(_ context.Context, _ pgx.Tx, id string) error { return q.add("push", id) }
func (q *queue) EnqueueVerify(_ context.Context, _ pgx.Tx, id string, _ payments.VerifySource) error {
	return q.add("verify", id)
}

func (q *queue) add(kind, id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.jobs = append(q.jobs, job{kind, id})
	return nil
}

func (q *queue) take() []job {
	q.mu.Lock()
	defer q.mu.Unlock()
	j := q.jobs
	q.jobs = nil
	return j
}

type env struct {
	pool *pgxpool.Pool
	clk  *clock.Manual
	mock *darajamock.Server
	q    *queue
	svc  *payments.Service
	ids  *identity.Service
	rg   *rg.Service
	// retries collects the snooze delays Verify asked for
	retries []time.Duration
}

var start = time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)

func setup(t *testing.T) *env {
	t.Helper()
	return setupWith(t, true)
}

func setupWith(t *testing.T, withDaraja bool) *env {
	t.Helper()
	pool := testpg.Pool(t, "app_api")
	e := &env{pool: pool, clk: clock.NewManual(start), q: &queue{}}
	aw := audit.NewWriter()
	e.ids = identity.NewService(pool, e.clk, nil, aw, identity.Config{AuthKey: []byte("0123456789abcdef0123456789abcdef")})
	e.rg = rg.NewService(pool, e.clk, aw, rg.Config{CoolingOff: 24 * time.Hour})
	ls := ledger.NewStore()

	if withDaraja {
		var svc *payments.Service
		cb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			if err := svc.HandleCallback(r.Context(), raw); err != nil {
				t.Errorf("callback: %v", err)
			}
		}))
		t.Cleanup(cb.Close)
		e.mock = darajamock.New(darajamock.Options{ConsumerKey: "ck", ConsumerSecret: "cs", ShortCode: "174379", Passkey: "pk", HTTPClient: cb.Client()})
		ms := httptest.NewServer(e.mock)
		t.Cleanup(ms.Close)
		c, err := daraja.New(daraja.Config{BaseURL: ms.URL, ConsumerKey: "ck", ConsumerSecret: "cs", ShortCode: "174379", Passkey: "pk", CallbackURL: cb.URL},
			ms.Client(), e.clk)
		if err != nil {
			t.Fatal(err)
		}
		svc = payments.NewService(pool, e.clk, c, e.q, e.ids, e.rg, wallet.NewService(ls), ls, aw, payments.Config{})
		e.svc = svc
		return e
	}
	e.svc = payments.NewService(pool, e.clk, nil, e.q, e.ids, e.rg, wallet.NewService(ls), ls, aw, payments.Config{})
	return e
}

var seq atomic.Int64

// player creates a KYC-verified account (phone not yet proved) and returns its id and claimed number.
func (e *env) player(t *testing.T) (string, string) {
	t.Helper()
	n := seq.Add(1)
	msisdn := fmt.Sprintf("2547%08d", n)
	var id string
	if err := e.pool.QueryRow(context.Background(),
		`INSERT INTO identity.users (msisdn, kyc_status, national_id_hash, password_hash) VALUES ($1, 'verified', $2, 'x') RETURNING id::text`,
		msisdn, []byte(fmt.Sprint("id-", n))).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id, msisdn
}

// run executes queued jobs until none are left, as the workers would. Verify snoozes are recorded, not repeated.
func (e *env) run(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	for range 10 {
		jobs := e.q.take()
		if len(jobs) == 0 {
			return
		}
		for _, j := range jobs {
			switch j.kind {
			case "push":
				if err := e.svc.Push(ctx, j.paymentID); err != nil {
					t.Fatalf("push job: %v", err)
				}
			case "verify":
				d, err := e.svc.Verify(ctx, j.paymentID)
				if err != nil {
					t.Fatalf("verify job: %v", err)
				}
				if d > 0 {
					e.retries = append(e.retries, d)
				}
			}
		}
	}
	t.Fatal("jobs keep enqueueing jobs")
}

func (e *env) topUp(t *testing.T, user string, amount int64) payments.TopUp {
	t.Helper()
	tu, err := e.svc.StartTopUp(context.Background(), user, amount, fmt.Sprint("key-", seq.Add(1)))
	if err != nil {
		t.Fatal(err)
	}
	return tu
}

// pushAndComplete runs the push job, lets the customer answer the prompt and runs whatever that enqueues.
func (e *env) pushAndComplete(t *testing.T) {
	t.Helper()
	e.run(t)
	p := e.mock.Pushes()
	if err := e.mock.Complete(context.Background(), p[len(p)-1].CheckoutRequestID); err != nil {
		t.Fatal(err)
	}
	e.run(t)
}

func (e *env) get(t *testing.T, user, id string) payments.TopUp {
	t.Helper()
	tu, err := e.svc.Get(context.Background(), user, id)
	if err != nil {
		t.Fatal(err)
	}
	return tu
}

func (e *env) balance(t *testing.T, user string) int64 {
	t.Helper()
	var b int64
	err := e.pool.QueryRow(context.Background(), `SELECT coalesce((SELECT balance_kes FROM wallet.wallets WHERE user_id = $1), 0)`, user).Scan(&b)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (e *env) phoneVerified(t *testing.T, user string) bool {
	t.Helper()
	var v bool
	if err := e.pool.QueryRow(context.Background(), `SELECT msisdn_verified_at IS NOT NULL FROM identity.users WHERE id = $1`, user).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func (e *env) status(t *testing.T, id string) (status string, reason *string) {
	t.Helper()
	if err := e.pool.QueryRow(context.Background(), `SELECT status, failure_reason FROM payments.payments WHERE id = $1`, id).Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	return status, reason
}

func TestTopUp_ConfirmedPaymentIsCreditedNetOfExciseAndProvesThePhone(t *testing.T) {
	e := setup(t)
	user, msisdn := e.player(t)
	tu := e.topUp(t, user, 100)
	if tu.Status != payments.StatusPending || tu.AmountRequestedKES != 100 {
		t.Fatalf("started = %+v", tu)
	}
	e.pushAndComplete(t)

	p := e.mock.Pushes()
	if len(p) != 1 || p[0].MSISDN != msisdn || p[0].AmountKES != 100 {
		t.Fatalf("pushes = %+v (the prompt must go to the claimed number)", p)
	}
	got := e.get(t, user, tu.ID)
	if got.Status != payments.StatusCredited || got.AmountConfirmedKES == nil || *got.AmountConfirmedKES != 100 {
		t.Fatalf("payment = %+v", got)
	}
	if b := e.balance(t, user); b != 95 { // 5% excise deducted from the deposit, owner decision 2026-10-22
		t.Fatalf("balance = %d, want 95", b)
	}
	rows, err := e.pool.Query(context.Background(), `SELECT kind, amount_kes FROM wallet.wallet_transactions WHERE user_id = $1 ORDER BY amount_kes DESC`, user)
	if err != nil {
		t.Fatal(err)
	}
	type wtx struct {
		kind string
		amt  int64
	}
	txs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (wtx, error) {
		var w wtx
		return w, r.Scan(&w.kind, &w.amt)
	})
	if err != nil || len(txs) != 2 || txs[0] != (wtx{"deposit", 100}) || txs[1] != (wtx{"deposit_excise", -5}) {
		t.Fatalf("wallet transactions = %+v %v", txs, err)
	}
	if !e.phoneVerified(t, user) {
		t.Fatal("approving the prompt did not prove the phone")
	}
}

func TestTopUp_IsRefusedUpFrontWhenTheAccountOrAmountDoesNotQualify(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	user, _ := e.player(t)

	for _, amt := range []int64{0, -1, payments.MinTopUpKES - 1, daraja.MaxSTKAmountKES + 1} {
		if _, err := e.svc.StartTopUp(ctx, user, amt, fmt.Sprint("amt-", amt)); !errors.Is(err, payments.ErrInvalidAmount) {
			t.Errorf("amount %d: err = %v", amt, err)
		}
	}

	unverified, _ := e.player(t)
	if _, err := e.pool.Exec(ctx, `UPDATE identity.users SET kyc_status = 'pending', national_id_hash = NULL WHERE id = $1`, unverified); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.StartTopUp(ctx, unverified, 100, "k1"); !errors.Is(err, identity.ErrKYCRequired) {
		t.Errorf("unverified: err = %v", err)
	}

	excluded, _ := e.player(t)
	if err := e.rg.SelfExclude(ctx, excluded, "6m"); err != nil {
		t.Fatal(err)
	}
	var re *rg.RestrictedError
	if _, err := e.svc.StartTopUp(ctx, excluded, 100, "k2"); !errors.As(err, &re) {
		t.Errorf("self-excluded: err = %v", err)
	}

	// a limit counts top-ups still in flight, so parallel requests cannot slip past it
	limited, _ := e.player(t)
	if _, err := e.rg.SetLimit(ctx, limited, rg.Daily, 150); err != nil {
		t.Fatal(err)
	}
	e.topUp(t, limited, 100)
	var le *rg.LimitError
	if _, err := e.svc.StartTopUp(ctx, limited, 60, "k3"); !errors.As(err, &le) || le.Remaining != 50 {
		t.Errorf("over the limit: err = %v %+v", err, le)
	}

	if n := len(e.q.take()); n != 1 {
		t.Fatalf("%d jobs enqueued, want only the one accepted top-up's", n)
	}
}

func TestTopUp_IsUnavailableWithoutDarajaOrWithoutAnExciseRule(t *testing.T) {
	ctx := context.Background()
	off := setupWith(t, false)
	user, _ := off.player(t)
	if _, err := off.svc.StartTopUp(ctx, user, 100, "k"); !errors.Is(err, payments.ErrUnavailable) {
		t.Fatalf("without daraja: err = %v", err)
	}

	e := setup(t)
	user, _ = e.player(t)
	e.clk.Set(time.Date(2025, 6, 30, 12, 0, 0, 0, time.UTC)) // before the first deposit_excise version
	if _, err := e.svc.StartTopUp(ctx, user, 100, "k"); !errors.Is(err, payments.ErrUnavailable) {
		t.Fatalf("without an excise rule: err = %v", err)
	}
}

func TestTopUp_TheSameKeyReturnsTheSamePayment(t *testing.T) {
	e := setup(t)
	user, _ := e.player(t)
	a, err := e.svc.StartTopUp(context.Background(), user, 100, "same")
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.svc.StartTopUp(context.Background(), user, 100, "same")
	if err != nil || a.ID != b.ID {
		t.Fatalf("a=%v b=%v err=%v", a.ID, b.ID, err)
	}
	if n := len(e.q.take()); n != 1 {
		t.Fatalf("%d push jobs, want 1", n)
	}
}

func TestTopUp_ACancelledPromptFailsAndCreditsNothing(t *testing.T) {
	e := setup(t)
	user, _ := e.player(t)
	e.mock.SetNextFault(darajamock.Fault{ResultCode: 1032})
	tu := e.topUp(t, user, 100)
	e.pushAndComplete(t)
	if got := e.get(t, user, tu.ID); got.Status != payments.StatusFailed {
		t.Fatalf("payment = %+v", got)
	}
	if st, _ := e.status(t, tu.ID); st != "cancelled" {
		t.Fatalf("status = %s", st)
	}
	if e.balance(t, user) != 0 || e.phoneVerified(t, user) {
		t.Fatal("a cancelled prompt moved money or proved the phone")
	}
}

func TestTopUp_DuplicateCallbacksAndRacingVerificationsCreditOnce(t *testing.T) {
	e := setup(t)
	user, _ := e.player(t)
	e.mock.SetNextFault(darajamock.Fault{DuplicateCallback: true})
	tu := e.topUp(t, user, 200)
	e.pushAndComplete(t)

	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := e.svc.Verify(context.Background(), tu.ID); err != nil {
				t.Errorf("verify: %v", err)
			}
		}()
	}
	wg.Wait()
	if b := e.balance(t, user); b != 190 {
		t.Fatalf("balance = %d, want 190 (credited once)", b)
	}
}

func TestTopUp_ACallbackIsNotTrustedUntilTheStatusQueryConfirmsIt(t *testing.T) {
	e := setup(t)
	user, _ := e.player(t)
	tu := e.topUp(t, user, 100)
	e.run(t) // push sent; the customer has not answered

	forged, _ := json.Marshal(map[string]any{"Body": map[string]any{"stkCallback": map[string]any{
		"MerchantRequestID": "x", "CheckoutRequestID": e.mock.Pushes()[0].CheckoutRequestID, "ResultCode": 0, "ResultDesc": "ok",
		"CallbackMetadata": map[string]any{"Item": []map[string]any{{"Name": "Amount", "Value": 100}, {"Name": "MpesaReceiptNumber", "Value": "FORGED1"}}},
	}}})
	if err := e.svc.HandleCallback(context.Background(), forged); err != nil {
		t.Fatal(err)
	}
	e.run(t)
	if b := e.balance(t, user); b != 0 {
		t.Fatalf("a forged callback was credited: balance %d", b)
	}
	if got := e.get(t, user, tu.ID); got.Status != payments.StatusPending || len(e.retries) == 0 {
		t.Fatalf("payment = %+v retries = %v (should wait for the query to settle)", got, e.retries)
	}
}

func TestTopUp_APaidPromptWhoseCallbackIsLostBecomesUnconfirmedNotCredited(t *testing.T) {
	e := setup(t)
	user, _ := e.player(t)
	e.mock.SetNextFault(darajamock.Fault{DropCallback: true})
	tu := e.topUp(t, user, 100)
	e.pushAndComplete(t)

	// the safety-net verification finds the payment paid but has no receipt yet: it waits
	if d, err := e.svc.Verify(context.Background(), tu.ID); err != nil || d == 0 {
		t.Fatalf("early verify: retry %v err %v", d, err)
	}
	e.clk.Advance(payments.DefaultExpiry + time.Second)
	if d, err := e.svc.Verify(context.Background(), tu.ID); err != nil || d != 0 {
		t.Fatalf("late verify: retry %v err %v", d, err)
	}
	if st, _ := e.status(t, tu.ID); st != "unconfirmed" {
		t.Fatalf("status = %s, want unconfirmed", st)
	}
	if e.balance(t, user) != 0 {
		t.Fatal("credited without a receipt")
	}
}

func TestTopUp_TheAmountCreditedIsTheAmountPaidNotTheAmountRequested(t *testing.T) {
	e := setup(t)
	user, _ := e.player(t)
	e.mock.SetNextFault(darajamock.Fault{PaidAmountKES: 40})
	tu := e.topUp(t, user, 100)
	e.pushAndComplete(t)
	got := e.get(t, user, tu.ID)
	if got.AmountConfirmedKES == nil || *got.AmountConfirmedKES != 40 || e.balance(t, user) != 38 {
		t.Fatalf("payment = %+v balance = %d", got, e.balance(t, user))
	}
}

func TestTopUp_AMaskedPayerNumberStillProvesThePushTarget(t *testing.T) {
	e := setup(t)
	user, _ := e.player(t)
	e.mock.SetNextFault(darajamock.Fault{MaskPhone: true})
	e.topUp(t, user, 100)
	e.pushAndComplete(t)
	if !e.phoneVerified(t, user) || e.balance(t, user) != 95 {
		t.Fatalf("verified=%v balance=%d", e.phoneVerified(t, user), e.balance(t, user))
	}
}

func TestTopUp_ANumberAnotherAccountAlreadyProvedIsCreditedButNotVerified(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	owner, msisdn := e.player(t)
	if err := pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error { return e.ids.MarkPhoneVerified(ctx, tx, owner, msisdn) }); err != nil {
		t.Fatal(err)
	}
	squatter, _ := e.player(t)
	if _, err := e.pool.Exec(ctx, `UPDATE identity.users SET msisdn = $1 WHERE id = $2`, msisdn, squatter); err != nil {
		t.Fatal(err)
	}
	e.topUp(t, squatter, 100)
	e.pushAndComplete(t)
	if e.balance(t, squatter) != 95 {
		t.Fatalf("the money was paid and must be credited: balance %d", e.balance(t, squatter))
	}
	if e.phoneVerified(t, squatter) {
		t.Fatal("a second account proved an already-proved number")
	}
}

func TestTopUp_APushWhoseOutcomeIsUnknownFailsAndIsNeverRetried(t *testing.T) {
	e := setup(t)
	user, _ := e.player(t)
	e.mock.SetNextFault(darajamock.Fault{PushStatus: http.StatusInternalServerError})
	tu := e.topUp(t, user, 100)
	e.run(t)
	st, reason := e.status(t, tu.ID)
	if st != "failed" || reason == nil || *reason != "push_outcome_unknown" {
		t.Fatalf("status = %s %v", st, reason)
	}
	if err := e.svc.Push(context.Background(), tu.ID); err != nil {
		t.Fatal(err)
	}
	if n := len(e.mock.Pushes()); n != 0 {
		t.Fatalf("%d prompts reached the customer after an unknown outcome", n)
	}
}

func TestTopUp_AReceiptAlreadyCreditedOnAnotherPaymentIsRefused(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	user, _ := e.player(t)
	e.topUp(t, user, 100)
	e.pushAndComplete(t)
	var receipt string
	if err := e.pool.QueryRow(ctx, `SELECT mpesa_receipt FROM payments.payments WHERE user_id = $1`, user).Scan(&receipt); err != nil {
		t.Fatal(err)
	}

	e.mock.SetNextFault(darajamock.Fault{DropCallback: true})
	second := e.topUp(t, user, 100)
	e.pushAndComplete(t) // paid at M-Pesa, but its own callback is lost...
	replay, _ := json.Marshal(map[string]any{"Body": map[string]any{"stkCallback": map[string]any{
		"CheckoutRequestID": e.mock.Pushes()[1].CheckoutRequestID, "ResultCode": 0, "ResultDesc": "ok",
		"CallbackMetadata": map[string]any{"Item": []map[string]any{{"Name": "Amount", "Value": 100}, {"Name": "MpesaReceiptNumber", "Value": receipt}}},
	}}})
	if err := e.svc.HandleCallback(ctx, replay); err != nil { // ...and someone replays the first one's receipt
		t.Fatal(err)
	}
	e.run(t)
	if st, reason := e.status(t, second.ID); st != "unconfirmed" || reason == nil || *reason != "receipt_reused" {
		t.Fatalf("status = %s %v", st, reason)
	}
	if b := e.balance(t, user); b != 95 {
		t.Fatalf("balance = %d, want only the first deposit", b)
	}
}

func TestCallback_ForAnUnknownCheckoutIsKeptForReconciliation(t *testing.T) {
	e := setup(t)
	body := []byte(`{"Body":{"stkCallback":{"CheckoutRequestID":"ws_CO_unknown","ResultCode":1032,"ResultDesc":"cancelled"}}}`)
	if err := e.svc.HandleCallback(context.Background(), body); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM payments.stk_orphan_callbacks WHERE checkout_request_id = 'ws_CO_unknown'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("orphans = %d %v", n, err)
	}
	if err := e.svc.HandleCallback(context.Background(), []byte(`<html>`)); !errors.Is(err, daraja.ErrBadCallback) {
		t.Fatalf("garbage: %v", err)
	}
}

func TestSweep_VerifiesPaymentsThatHaveBeenPendingTooLong(t *testing.T) {
	e := setup(t)
	user, _ := e.player(t)
	e.mock.SetNextFault(darajamock.Fault{DropCallback: true})
	tu := e.topUp(t, user, 100)
	e.run(t)
	if err := e.svc.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if jobs := e.q.take(); len(jobs) != 0 {
		t.Fatalf("swept a fresh payment: %v", jobs)
	}
	e.clk.Advance(payments.DefaultVerifyAfter + time.Second)
	if err := e.svc.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if jobs := e.q.take(); len(jobs) != 1 || jobs[0] != (job{"verify", tu.ID}) {
		t.Fatalf("jobs = %v", jobs)
	}
}

func TestGet_AnotherPlayersPaymentIsNotFound(t *testing.T) {
	e := setup(t)
	a, _ := e.player(t)
	b, _ := e.player(t)
	tu := e.topUp(t, a, 100)
	if _, err := e.svc.Get(context.Background(), b, tu.ID); !errors.Is(err, payments.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	if _, err := e.svc.Get(context.Background(), a, "not-a-uuid"); !errors.Is(err, payments.ErrNotFound) {
		t.Fatalf("bad id err = %v", err)
	}
}

func TestTopUp_ACallbackThatBeatThePushBookkeepingIsStillUsed(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	user, msisdn := e.player(t)
	e.mock.SetNextFault(darajamock.Fault{DropCallback: true})
	tu := e.topUp(t, user, 100)
	e.pushAndComplete(t)
	// Daraja called back before the push job had recorded the checkout id, so the callback was kept as an orphan.
	checkout := e.mock.Pushes()[0].CheckoutRequestID
	raw, _ := json.Marshal(map[string]any{"Body": map[string]any{"stkCallback": map[string]any{
		"CheckoutRequestID": checkout, "ResultCode": 0, "ResultDesc": "ok",
		"CallbackMetadata": map[string]any{"Item": []map[string]any{{"Name": "Amount", "Value": 100}, {"Name": "MpesaReceiptNumber", "Value": "EARLY1"},
			{"Name": "PhoneNumber", "Value": json.Number(msisdn)}}},
	}}})
	if _, err := e.pool.Exec(ctx, `INSERT INTO payments.stk_orphan_callbacks (checkout_request_id, raw_payload) VALUES ($1, $2)`, checkout, raw); err != nil {
		t.Fatal(err)
	}
	if d, err := e.svc.Verify(ctx, tu.ID); err != nil || d != 0 {
		t.Fatalf("verify: retry %v err %v", d, err)
	}
	if got := e.get(t, user, tu.ID); got.Status != payments.StatusCredited || e.balance(t, user) != 95 {
		t.Fatalf("payment = %+v balance = %d", got, e.balance(t, user))
	}
}

func TestTopUp_APayerNumberThatDiffersFromThePushTargetIsCreditedButProvesNothing(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	user, _ := e.player(t)
	e.mock.SetNextFault(darajamock.Fault{DropCallback: true})
	tu := e.topUp(t, user, 100)
	e.pushAndComplete(t)
	raw, _ := json.Marshal(map[string]any{"Body": map[string]any{"stkCallback": map[string]any{
		"CheckoutRequestID": e.mock.Pushes()[0].CheckoutRequestID, "ResultCode": 0, "ResultDesc": "ok",
		"CallbackMetadata": map[string]any{"Item": []map[string]any{{"Name": "Amount", "Value": 100}, {"Name": "MpesaReceiptNumber", "Value": "OTHER1"},
			{"Name": "PhoneNumber", "Value": json.Number("254799999999")}}},
	}}})
	if err := e.svc.HandleCallback(ctx, raw); err != nil {
		t.Fatal(err)
	}
	e.run(t)
	if got := e.get(t, user, tu.ID); got.Status != payments.StatusCredited || e.balance(t, user) != 95 {
		t.Fatalf("payment = %+v balance = %d", got, e.balance(t, user))
	}
	if e.phoneVerified(t, user) {
		t.Fatal("a payment reported from another number proved the account's phone")
	}
}

// startPushed starts a top-up and runs only its push job, leaving the prompt unanswered.
func (e *env) startPushed(t *testing.T, f darajamock.Fault) (string, payments.TopUp) {
	t.Helper()
	user, _ := e.player(t)
	e.mock.SetNextFault(f)
	tu := e.topUp(t, user, 100)
	e.q.take()
	if err := e.svc.Push(context.Background(), tu.ID); err != nil {
		t.Fatal(err)
	}
	e.q.take()
	return user, tu
}

func TestVerify_SettlesWhatItCannotConfirmOnceThePaymentExpires(t *testing.T) {
	ctx := context.Background()
	cases := map[string]struct {
		fault      darajamock.Fault
		push       bool
		wantStatus string
		wantReason string
	}{
		"push never recorded":   {push: false, wantStatus: "failed", wantReason: "push_outcome_unknown"},
		"query keeps refusing":  {push: true, fault: darajamock.Fault{QueryStatus: http.StatusBadRequest}, wantStatus: "unconfirmed", wantReason: "query_rejected"},
		"prompt never answered": {push: true, wantStatus: "unconfirmed", wantReason: "still_pending_at_expiry"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			var tu payments.TopUp
			if tc.push {
				_, tu = e.startPushed(t, tc.fault)
			} else {
				user, _ := e.player(t)
				tu = e.topUp(t, user, 100)
				e.q.take() // the push job never ran
			}
			if d, err := e.svc.Verify(ctx, tu.ID); err != nil || d != payments.DefaultVerifyAfter {
				t.Fatalf("before expiry: retry %v err %v", d, err)
			}
			e.clk.Advance(payments.DefaultExpiry + time.Second)
			if d, err := e.svc.Verify(ctx, tu.ID); err != nil || d != 0 {
				t.Fatalf("after expiry: retry %v err %v", d, err)
			}
			if st, reason := e.status(t, tu.ID); st != tc.wantStatus || reason == nil || *reason != tc.wantReason {
				t.Fatalf("status = %s %v", st, reason)
			}
			if d, err := e.svc.Verify(ctx, tu.ID); err != nil || d != 0 { // settled payments are left alone
				t.Fatalf("again: %v %v", d, err)
			}
		})
	}
}

func TestVerify_AQueryOutageIsRetriedWithBackoff(t *testing.T) {
	e := setup(t)
	_, tu := e.startPushed(t, darajamock.Fault{QueryStatus: http.StatusServiceUnavailable})
	if _, err := e.svc.Verify(context.Background(), tu.ID); !errors.Is(err, daraja.ErrUnavailable) {
		t.Fatalf("err = %v, want an outage for the job to retry", err)
	}
	if st, _ := e.status(t, tu.ID); st != "stk_sent" {
		t.Fatalf("status = %s", st)
	}
}

func TestJobs_FailClosedWhenDarajaIsNotConfigured(t *testing.T) {
	e := setupWith(t, false)
	ctx := context.Background()
	user, msisdn := e.player(t)
	var id string
	if err := e.pool.QueryRow(ctx, `INSERT INTO payments.payments (user_id, channel, amount_requested_kes, msisdn, idempotency_key)
		VALUES ($1, 'stk', 100, $2, 'k') RETURNING id::text`, user, msisdn).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Push(ctx, id); !errors.Is(err, payments.ErrUnavailable) {
		t.Fatalf("push: %v", err)
	}
	if _, err := e.svc.Verify(ctx, id); !errors.Is(err, payments.ErrUnavailable) {
		t.Fatalf("verify: %v", err)
	}
}

// spend takes amount out of the player's wallet the way a play would (phase 3), so reversals can find it spent.
func (e *env) spend(t *testing.T, user string, amount int64) {
	t.Helper()
	p, err := ledger.Stake(fmt.Sprintf("00000000-0000-7000-8000-%012d", seq.Add(1)), user, amount)
	if err != nil {
		t.Fatal(err)
	}
	if err := pgx.BeginFunc(context.Background(), e.pool, func(tx pgx.Tx) error {
		_, err := wallet.NewService(ledger.NewStore()).Apply(context.Background(), tx, p)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func (e *env) debt(t *testing.T, user string) int64 {
	t.Helper()
	var d int64
	if err := pgx.BeginFunc(context.Background(), e.pool, func(tx pgx.Tx) error {
		var err error
		d, err = wallet.NewService(ledger.NewStore()).Debt(context.Background(), tx, user)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return d
}

func (e *env) receipt(t *testing.T, id string) string {
	t.Helper()
	var r string
	if err := e.pool.QueryRow(context.Background(), `SELECT mpesa_receipt FROM payments.payments WHERE id = $1`, id).Scan(&r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestReverse_TakesBackWhatThePlayerReceivedBooksTheRestAsDebtAndTheNextTopUpRecoversIt(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	user, _ := e.player(t)
	first := e.topUp(t, user, 100)
	e.pushAndComplete(t) // credited 95 (5 excise deducted)
	e.spend(t, user, 60) // 35 left

	res, err := e.svc.Reverse(ctx, e.receipt(t, first.ID), "customer dispute upheld by Safaricom", "operator:test")
	if err != nil {
		t.Fatal(err)
	}
	if res.PaymentID != first.ID || res.UserID != user || res.TakenKES != 35 || res.DebtKES != 60 {
		t.Fatalf("reversal = %+v, want 35 taken and 60 owed", res)
	}
	if st, _ := e.status(t, first.ID); st != "reversed" || e.balance(t, user) != 0 || e.debt(t, user) != 60 {
		t.Fatalf("after reversal: status %s balance %d debt %d", st, e.balance(t, user), e.debt(t, user))
	}
	if got := e.get(t, user, first.ID); got.Status != payments.StatusReversed {
		t.Fatalf("player sees %q", got.Status)
	}

	e.topUp(t, user, 100) // credited 95, of which 60 repays the debt
	e.pushAndComplete(t)
	if b, d := e.balance(t, user), e.debt(t, user); b != 35 || d != 0 {
		t.Fatalf("after the next top-up: balance %d debt %d, want 35 and 0", b, d)
	}
}

func TestReverse_IsRefusedForUnknownUncreditedOrAlreadyReversedPayments(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	if _, err := e.svc.Reverse(ctx, "NOSUCHRCPT", "x", "operator:test"); !errors.Is(err, payments.ErrNotFound) {
		t.Fatalf("unknown receipt: %v", err)
	}
	user, _ := e.player(t)
	tu := e.topUp(t, user, 100)
	e.pushAndComplete(t)
	r := e.receipt(t, tu.ID)
	if _, err := e.svc.Reverse(ctx, r, "", "operator:test"); !errors.Is(err, payments.ErrReasonRequired) {
		t.Fatalf("no reason: %v", err)
	}
	if _, err := e.svc.Reverse(ctx, r, "dispute", "operator:test"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Reverse(ctx, r, "dispute", "operator:test"); !errors.Is(err, payments.ErrNotReversible) {
		t.Fatalf("second reversal: %v", err)
	}
	var audits int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM audit.audit_log WHERE action = 'payment.topup.reversed' AND actor_type = 'staff' AND actor_id = 'operator:test'`).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("audit rows = %d %v", audits, err)
	}
}

func TestResolveUnconfirmed_CreditsFromTheStatementReceiptExactlyOnce(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	user, _ := e.player(t)
	e.mock.SetNextFault(darajamock.Fault{DropCallback: true})
	tu := e.topUp(t, user, 100)
	e.pushAndComplete(t)
	e.clk.Advance(payments.DefaultExpiry + time.Second)
	if _, err := e.svc.Verify(ctx, tu.ID); err != nil {
		t.Fatal(err)
	}
	if st, _ := e.status(t, tu.ID); st != "unconfirmed" {
		t.Fatalf("status = %s", st)
	}

	for _, amt := range []int64{0, -5, daraja.MaxSTKAmountKES + 1} {
		if err := e.svc.ResolveUnconfirmed(ctx, tu.ID, "STMT0001", amt, "operator:test"); !errors.Is(err, payments.ErrInvalidAmount) {
			t.Fatalf("amount %d: %v", amt, err)
		}
	}
	if err := e.svc.ResolveUnconfirmed(ctx, tu.ID, "STMT0001", 100, "operator:test"); err != nil {
		t.Fatal(err)
	}
	if got := e.get(t, user, tu.ID); got.Status != payments.StatusCredited || e.balance(t, user) != 95 || !e.phoneVerified(t, user) {
		t.Fatalf("payment %+v balance %d phone %v", got, e.balance(t, user), e.phoneVerified(t, user))
	}
	if err := e.svc.ResolveUnconfirmed(ctx, tu.ID, "STMT0001", 100, "operator:test"); !errors.Is(err, payments.ErrNotResolvable) {
		t.Fatalf("second resolve: %v", err)
	}

	// a receipt that already credited a payment cannot credit another
	other, _ := e.player(t)
	e.mock.SetNextFault(darajamock.Fault{DropCallback: true})
	tu2 := e.topUp(t, other, 100)
	e.pushAndComplete(t)
	e.clk.Advance(payments.DefaultExpiry + time.Second)
	if _, err := e.svc.Verify(ctx, tu2.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.ResolveUnconfirmed(ctx, tu2.ID, "STMT0001", 100, "operator:test"); !errors.Is(err, payments.ErrReceiptUsed) {
		t.Fatalf("reused receipt: %v", err)
	}
	if err := e.svc.ResolveUnconfirmed(ctx, "0192f000-0000-7000-8000-000000000000", "STMT0002", 100, "operator:test"); !errors.Is(err, payments.ErrNotFound) {
		t.Fatalf("unknown payment: %v", err)
	}
}

func (e *env) reconcile(t *testing.T) payments.Report {
	t.Helper()
	r, err := e.svc.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestReconcile_IsCleanAfterNormalTrafficAndListsWhatNeedsAnOperator(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	if r := e.reconcile(t); !r.OK || len(r.Mismatches) != 0 {
		t.Fatalf("empty database: %+v", r)
	}
	a, _ := e.player(t)
	paid := e.topUp(t, a, 100)
	e.pushAndComplete(t)
	e.spend(t, a, 90)
	if _, err := e.svc.Reverse(ctx, e.receipt(t, paid.ID), "dispute", "operator:test"); err != nil { // leaves debt
		t.Fatal(err)
	}
	b, _ := e.player(t)
	e.mock.SetNextFault(darajamock.Fault{DropCallback: true})
	lost := e.topUp(t, b, 50)
	e.pushAndComplete(t)
	e.clk.Advance(payments.DefaultExpiry + time.Second)
	if _, err := e.svc.Verify(ctx, lost.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.HandleCallback(ctx, []byte(`{"Body":{"stkCallback":{"CheckoutRequestID":"ws_CO_orphan","ResultCode":1032,"ResultDesc":"x"}}}`)); err != nil {
		t.Fatal(err)
	}

	r := e.reconcile(t)
	if !r.OK || len(r.Mismatches) != 0 {
		t.Fatalf("normal traffic produced mismatches: %+v", r.Mismatches)
	}
	if got := r.Attention["unconfirmed"]; len(got) != 1 || got[0] != lost.ID {
		t.Fatalf("unconfirmed = %v", got)
	}
	if got := r.Attention["orphan_callbacks"]; len(got) != 1 || got[0] != "ws_CO_orphan" {
		t.Fatalf("orphans = %v", got)
	}
	if got := r.Attention["players_in_debt"]; len(got) != 1 || got[0] != a {
		t.Fatalf("debtors = %v", got)
	}
	if r.Totals["clearing_kes"] != 0 || r.Totals["credited_kes"] != 0 || r.Totals["debt_kes"] != 90 {
		t.Fatalf("totals = %v (the only credited top-up was reversed: 95 received, 90 spent, 5 taken back, 90 owed)", r.Totals)
	}
}

func TestReconcile_FindsACreditedPaymentTheLedgerNeverSaw(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	user, msisdn := e.player(t)
	var id string
	if err := e.pool.QueryRow(ctx, `INSERT INTO payments.payments (user_id, channel, status, amount_requested_kes, amount_confirmed_kes,
		msisdn, mpesa_receipt, idempotency_key) VALUES ($1, 'stk', 'credited', 100, 100, $2, 'GHOST1', 'k') RETURNING id::text`, user, msisdn).Scan(&id); err != nil {
		t.Fatal(err)
	}
	r := e.reconcile(t)
	if r.OK {
		t.Fatal("a credit without a deposit journal passed reconciliation")
	}
	if got := r.Mismatches["credited_without_deposit"]; len(got) != 1 || got[0] != id {
		t.Fatalf("credited_without_deposit = %v", got)
	}
	if got := r.Mismatches["clearing_vs_credited"]; len(got) != 1 {
		t.Fatalf("clearing_vs_credited = %v", got)
	}
	var stored int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM payments.reconciliation_runs WHERE NOT ok`).Scan(&stored); err != nil || stored != 1 {
		t.Fatalf("stored failing runs = %d %v", stored, err)
	}
}

// Every payments operation must report a failed database call as an error, never as success or "nothing to do".
func TestPayments_DatabaseFailuresAreReportedNotSwallowed(t *testing.T) {
	e := setup(t)
	user, _ := e.player(t)
	tu := e.topUp(t, user, 100)
	dead, cancel := context.WithCancel(context.Background())
	cancel()
	cbBody := []byte(`{"Body":{"stkCallback":{"CheckoutRequestID":"ws_CO_x","ResultCode":1032,"ResultDesc":"x"}}}`)
	for name, op := range map[string]func() error{
		"start":     func() error { _, err := e.svc.StartTopUp(dead, user, 100, "dead-key"); return err },
		"get":       func() error { _, err := e.svc.Get(dead, user, tu.ID); return err },
		"push":      func() error { return e.svc.Push(dead, tu.ID) },
		"callback":  func() error { return e.svc.HandleCallback(dead, cbBody) },
		"verify":    func() error { _, err := e.svc.Verify(dead, tu.ID); return err },
		"sweep":     func() error { return e.svc.Sweep(dead) },
		"reconcile": func() error { _, err := e.svc.Reconcile(dead); return err },
		"reverse":   func() error { _, err := e.svc.Reverse(dead, "R1", "x", "operator:t"); return err },
		"resolve":   func() error { return e.svc.ResolveUnconfirmed(dead, tu.ID, "R1", 100, "operator:t") },
	} {
		if err := op(); err == nil {
			t.Errorf("%s: a failed database call was reported as success", name)
		}
	}
}
