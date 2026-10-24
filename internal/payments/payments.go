// Package payments takes money in through M-Pesa. A top-up is an STK Push to the number the account claims: the
// request path only records the intent and enqueues a job; the job sends the prompt; Daraja's callback is stored and
// triggers verification; verification confirms the outcome with Daraja's status query and only then credits the
// confirmed amount, net of deposit excise, and records that the player proved the phone (ADR 0005). No network call
// ever runs inside a database transaction, and nothing is credited on a callback alone.
package payments

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/itsivali/lottery-platform/internal/audit"
	"github.com/itsivali/lottery-platform/internal/identity"
	"github.com/itsivali/lottery-platform/internal/ledger"
	"github.com/itsivali/lottery-platform/internal/payments/daraja"
	"github.com/itsivali/lottery-platform/internal/payments/store"
	"github.com/itsivali/lottery-platform/internal/platform/clock"
)

// Errors.
var (
	ErrInvalidAmount = errors.New("payments: invalid amount")
	ErrUnavailable   = errors.New("payments: top-ups are unavailable")
	ErrNotFound      = errors.New("payments: not found")
)

// Bounds and timings. MinTopUpKES is a placeholder (open question: minimum verification deposit, suggested KES 10).
const (
	MinTopUpKES        int64 = 10
	DefaultVerifyAfter       = 2 * time.Minute  // safety-net verification when no callback has arrived
	DefaultExpiry            = 15 * time.Minute // after this a payment without a usable outcome is settled as unconfirmed/failed
	exciseCode               = "deposit_excise"
	// Excise is deducted from the deposit and shown as its own wallet line (owner decision, 2026-10-22).
	deductExcise = true
)

// Player-facing states. The internal status set is richer (see the payments_status_check constraint).
const (
	StatusPending  = "pending"
	StatusCredited = "credited"
	StatusFailed   = "failed"
)

// TopUp is what the player sees of a payment.
type TopUp struct {
	ID                 string
	Status             string
	AmountRequestedKES int64
	AmountConfirmedKES *int64
	CreatedAt          time.Time
}

func publicStatus(s string) string {
	switch s {
	case "credited":
		return StatusCredited
	case "reversed":
		return StatusReversed
	case "failed", "cancelled", "expired":
		return StatusFailed
	default: // initiated, stk_sent, callback_received, verified, unconfirmed: still being settled
		return StatusPending
	}
}

func final(s string) bool {
	switch s {
	case "credited", "failed", "cancelled", "expired", "unconfirmed", "reversed":
		return true
	}
	return false
}

// Daraja is the part of the M-Pesa client the service uses.
type Daraja interface {
	STKPush(ctx context.Context, r daraja.STKPushRequest) (daraja.STKPushResult, error)
	STKQuery(ctx context.Context, checkoutRequestID string) (daraja.STKStatus, error)
}

// Enqueuer schedules jobs inside the caller's transaction, so a payment and its job commit together.
type Enqueuer interface {
	EnqueuePush(ctx context.Context, tx pgx.Tx, paymentID string) error
	// EnqueueVerify schedules verification. Jobs with the same payment and source collapse into one, so repeated
	// callbacks or sweeps do not pile up, while a callback is never held back behind a waiting safety-net job.
	EnqueueVerify(ctx context.Context, tx pgx.Tx, paymentID string, source VerifySource) error
	// EnqueueC2B schedules the verification of a paybill confirmation, collapsing duplicates the same way.
	EnqueueC2B(ctx context.Context, tx pgx.Tx, confirmationID string, source VerifySource) error
}

// VerifySource says what asked for a verification.
type VerifySource string

// Verification sources.
const (
	AfterPush     VerifySource = "push"
	AfterCallback VerifySource = "callback"
	FromSweep     VerifySource = "sweep"
	// AfterStatus is a paybill Transaction Status answer: its own source, so settlement never waits behind the
	// confirmation's job while that one is snoozed waiting for the answer.
	AfterStatus VerifySource = "status"
)

// Identity is what payments needs from the identity module.
type Identity interface {
	AssertCanTopUp(ctx context.Context, tx pgx.Tx, userID string) (string, error)
	ProvedPhoneOwner(ctx context.Context, tx pgx.Tx, msisdn string) (string, error)
	MarkPhoneVerified(ctx context.Context, tx pgx.Tx, userID, confirmedMSISDN string) error
}

// RG is what payments needs from responsible gaming.
type RG interface {
	AssertNotRestricted(ctx context.Context, tx pgx.Tx, userID string) error
	AssertDepositAllowed(ctx context.Context, tx pgx.Tx, userID string, amount int64, deposited func(since time.Time) (int64, error)) error
}

// Wallet applies postings and handles reversals and player debt.
type Wallet interface {
	Apply(ctx context.Context, tx pgx.Tx, p ledger.Posting) (int64, error)
	RecoverDebt(ctx context.Context, tx pgx.Tx, user, paymentID string) (int64, error)
	Debt(ctx context.Context, tx pgx.Tx, user string) (int64, error)
	ReverseDeposit(ctx context.Context, tx pgx.Tx, paymentID, user string, gross, exciseDeducted int64) (ledger.Posting, int64, error)
}

// Ledger returns the tax rule version in force and posts journals that touch no wallet (suspense).
type Ledger interface {
	ActiveRule(ctx context.Context, tx pgx.Tx, code string, on time.Time) (ledger.TaxRule, error)
	Post(ctx context.Context, tx pgx.Tx, j ledger.Journal) (string, error)
}

// Config tunes the service; zero values take the defaults.
type Config struct {
	VerifyAfter time.Duration
	Expiry      time.Duration
}

// Service runs top-ups.
type Service struct {
	pool   *pgxpool.Pool
	clk    clock.Clock
	daraja Daraja // nil: Daraja is not configured and top-ups are refused
	enq    Enqueuer
	ids    Identity
	rg     RG
	wallet Wallet
	rules  Ledger
	audit  *audit.Writer
	cfg    Config
	c2b    *c2bSetup // nil: paybill payments are not configured
}

// NewService returns a Service. A nil d (an untyped nil, not a nil *daraja.Client) disables top-ups.
func NewService(pool *pgxpool.Pool, clk clock.Clock, d Daraja, enq Enqueuer, ids Identity, r RG, w Wallet, rules Ledger, a *audit.Writer, cfg Config) *Service {
	if cfg.VerifyAfter <= 0 {
		cfg.VerifyAfter = DefaultVerifyAfter
	}
	if cfg.Expiry <= 0 {
		cfg.Expiry = DefaultExpiry
	}
	return &Service{pool: pool, clk: clk, daraja: d, enq: enq, ids: ids, rg: r, wallet: w, rules: rules, audit: a, cfg: cfg}
}

// StartTopUp records a top-up and enqueues its STK push. key is the client's idempotency key: the same key returns
// the same payment. The prompt always goes to the account's claimed number.
func (s *Service) StartTopUp(ctx context.Context, userID string, amount int64, key string) (TopUp, error) {
	if amount < MinTopUpKES || amount > daraja.MaxSTKAmountKES {
		return TopUp{}, ErrInvalidAmount
	}
	if s.daraja == nil {
		return TopUp{}, ErrUnavailable
	}
	var id string
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		// one top-up decision at a time per player, so limits see every request in flight
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext('payments.topup'), hashtext($1))", userID); err != nil {
			return err
		}
		existing, err := q.GetPaymentByKey(ctx, store.GetPaymentByKeyParams{UserID: userID, IdempotencyKey: key})
		if err == nil {
			id = existing
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		now, err := s.clk.Now(ctx, tx)
		if err != nil {
			return err
		}
		msisdn, err := s.ids.AssertCanTopUp(ctx, tx, userID)
		if err != nil {
			return err
		}
		if err := s.rg.AssertNotRestricted(ctx, tx, userID); err != nil {
			return err
		}
		if _, err := s.rules.ActiveRule(ctx, tx, exciseCode, now); errors.Is(err, ledger.ErrNoTaxRule) {
			return ErrUnavailable
		} else if err != nil {
			return err
		}
		if err := s.rg.AssertDepositAllowed(ctx, tx, userID, amount, func(since time.Time) (int64, error) {
			return q.DepositedSince(ctx, store.DepositedSinceParams{UserID: userID, CreatedAt: since})
		}); err != nil {
			return err
		}
		if id, err = q.InsertTopUp(ctx, store.InsertTopUpParams{UserID: userID, AmountKes: amount, Msisdn: &msisdn, IdempotencyKey: key, At: now}); err != nil {
			return err
		}
		if err := s.enq.EnqueuePush(ctx, tx, id); err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, audit.Event{ActorType: audit.Player, ActorID: userID, Action: "payment.topup.started",
			EntityType: "payment", EntityID: id, Details: map[string]any{"amount_kes": amount}})
	})
	if err != nil {
		return TopUp{}, err
	}
	return s.Get(ctx, userID, id)
}

// Get returns one of the player's payments. Another player's payment is ErrNotFound.
func (s *Service) Get(ctx context.Context, userID, id string) (TopUp, error) {
	p, err := store.New(s.pool).GetPayment(ctx, id)
	var pe *pgconn.PgError
	if errors.Is(err, pgx.ErrNoRows) || (errors.As(err, &pe) && pe.Code == "22P02") || (err == nil && p.UserID != userID) {
		return TopUp{}, ErrNotFound
	}
	if err != nil {
		return TopUp{}, err
	}
	return TopUp{ID: p.ID, Status: publicStatus(p.Status), AmountRequestedKES: p.AmountRequestedKes, AmountConfirmedKES: p.AmountConfirmedKes, CreatedAt: p.CreatedAt}, nil
}

// RecentWindow is how far back Recent looks for payments that already settled.
const RecentWindow = 15 * time.Minute

// Recent returns the player's payments that are still being settled or changed within RecentWindow, newest first.
// A live stream starts from it, so a top-up that settled between its request and the stream's start is not missed.
func (s *Service) Recent(ctx context.Context, userID string) ([]TopUp, error) {
	now, err := s.clk.Now(ctx, s.pool)
	if err != nil {
		return nil, err
	}
	rows, err := store.New(s.pool).RecentPayments(ctx, store.RecentPaymentsParams{UserID: userID, Since: now.Add(-RecentWindow)})
	if err != nil {
		return nil, err
	}
	out := make([]TopUp, 0, len(rows))
	for _, p := range rows {
		out = append(out, TopUp{ID: p.ID, Status: publicStatus(p.Status), AmountRequestedKES: p.AmountRequestedKes, AmountConfirmedKES: p.AmountConfirmedKes, CreatedAt: p.CreatedAt})
	}
	return out, nil
}

// Push sends the STK prompt for a recorded top-up. It never retries: a push whose outcome is unknown (a timeout,
// a 5xx) may still have reached the customer, and a second prompt could take a second payment we would not know
// about. Such a payment fails; any callback it later produces is kept as an orphan for reconciliation.
func (s *Service) Push(ctx context.Context, paymentID string) error {
	if s.daraja == nil {
		return ErrUnavailable
	}
	p, err := store.New(s.pool).GetPayment(ctx, paymentID)
	if err != nil {
		return err
	}
	if p.Status != "initiated" || p.Msisdn == nil {
		return nil
	}
	res, pushErr := s.daraja.STKPush(ctx, daraja.STKPushRequest{MSISDN: *p.Msisdn, AmountKES: p.AmountRequestedKes, AccountRef: "TOPUP", Description: "Wallet top-up"})
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		now, err := s.clk.Now(ctx, tx)
		if err != nil {
			return err
		}
		q := store.New(tx)
		if pushErr != nil {
			reason := "push_outcome_unknown"
			if errors.Is(pushErr, daraja.ErrRejected) || errors.Is(pushErr, daraja.ErrInvalidRequest) {
				reason = "push_rejected"
			}
			slog.WarnContext(ctx, "stk push failed; the top-up fails", "payment", paymentID, "reason", reason, "err", pushErr)
			return s.settle(ctx, tx, p.UserID, paymentID, "failed", reason, nil, now)
		}
		if _, err := q.MarkSTKSent(ctx, store.MarkSTKSentParams{MerchantRequestID: &res.MerchantRequestID, CheckoutRequestID: &res.CheckoutRequestID, At: now, ID: paymentID}); err != nil {
			return err
		}
		return s.enq.EnqueueVerify(ctx, tx, paymentID, AfterPush)
	})
}

// HandleCallback stores a Daraja STK callback and enqueues its verification. It moves no money. A callback for an
// unknown checkout is kept as an orphan. A malformed body returns daraja.ErrBadCallback.
func (s *Service) HandleCallback(ctx context.Context, raw []byte) error {
	cb, err := daraja.ParseSTKCallback(raw)
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		p, err := q.LockPaymentByCheckout(ctx, &cb.CheckoutRequestID)
		if errors.Is(err, pgx.ErrNoRows) {
			return q.InsertOrphanCallback(ctx, store.InsertOrphanCallbackParams{CheckoutRequestID: cb.CheckoutRequestID, RawPayload: raw})
		}
		if err != nil {
			return err
		}
		if final(p.Status) {
			return nil
		}
		now, err := s.clk.Now(ctx, tx)
		if err != nil {
			return err
		}
		if err := q.RecordCallback(ctx, store.RecordCallbackParams{RawCallback: raw, At: now, ID: p.ID}); err != nil {
			return err
		}
		return s.enq.EnqueueVerify(ctx, tx, p.ID, AfterCallback)
	})
}

// Verify settles a payment from Daraja's status query. It returns a positive duration when the outcome is not known
// yet and the job should run again after it; an error is an outage to retry with backoff.
//
// Without a callback it does not ask before VerifyAfter has passed since the payment started: the customer cannot have
// answered the prompt yet, and Daraja throttles early queries (HTTP 429, seen live 2026-10-24). A query that finds
// Daraja throttling or unavailable is asked again after VerifyAfter until the payment expires; after that the outage
// is returned so the job keeps retrying with backoff.
func (s *Service) Verify(ctx context.Context, paymentID string) (time.Duration, error) {
	p, err := store.New(s.pool).GetPayment(ctx, paymentID)
	if err != nil {
		return 0, err
	}
	if final(p.Status) {
		return 0, nil
	}
	now, err := s.clk.Now(ctx, s.pool)
	if err != nil {
		return 0, err
	}
	expired := now.Sub(p.CreatedAt) > s.cfg.Expiry
	if s.daraja == nil { // configuration removed while payments were in flight: retry until it is back
		return 0, ErrUnavailable
	}
	if age := now.Sub(p.CreatedAt); age < s.cfg.VerifyAfter {
		heard, err := s.calledBack(ctx, p.RawCallback, p.CheckoutRequestID)
		if err != nil {
			return 0, err
		}
		if !heard {
			return s.cfg.VerifyAfter - age, nil
		}
	}
	if p.CheckoutRequestID == nil { // the push never recorded an acceptance
		if !expired {
			return s.cfg.VerifyAfter, nil
		}
		return 0, s.settleTx(ctx, paymentID, "failed", "push_outcome_unknown", nil)
	}
	st, err := s.daraja.STKQuery(ctx, *p.CheckoutRequestID)
	switch {
	case errors.Is(err, daraja.ErrRejected):
		if !expired {
			return s.cfg.VerifyAfter, nil
		}
		return 0, s.settleTx(ctx, paymentID, "unconfirmed", "query_rejected", nil)
	case err != nil && !expired:
		slog.WarnContext(ctx, "stk status query failed; asking again later", "payment", paymentID, "err", err, "in", s.cfg.VerifyAfter)
		return s.cfg.VerifyAfter, nil
	case err != nil:
		return 0, err
	}
	code := st.ResultCode
	switch st.State {
	case daraja.Pending:
		if !expired {
			return s.cfg.VerifyAfter, nil
		}
		return 0, s.settleTx(ctx, paymentID, "unconfirmed", "still_pending_at_expiry", nil)
	case daraja.Failed:
		status := "failed"
		if st.ResultCode == 1032 {
			status = "cancelled"
		}
		return 0, s.settleTx(ctx, paymentID, status, "", &code)
	}
	return s.credit(ctx, paymentID, expired)
}

// calledBack reports whether Daraja has sent a callback for the payment, including one kept as an orphan because it
// arrived before the push job recorded the checkout id.
func (s *Service) calledBack(ctx context.Context, raw []byte, checkoutID *string) (bool, error) {
	if raw != nil {
		return true, nil
	}
	if checkoutID == nil {
		return false, nil
	}
	_, err := store.New(s.pool).OrphanCallback(ctx, *checkoutID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// credit runs after the status query said the customer paid. The amount and receipt come from the stored callback;
// without one there is nothing safe to credit yet.
func (s *Service) credit(ctx context.Context, paymentID string, expired bool) (time.Duration, error) {
	var retry time.Duration
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		p, err := q.LockPayment(ctx, paymentID)
		if err != nil {
			return err
		}
		if final(p.Status) {
			return nil
		}
		now, err := s.clk.Now(ctx, tx)
		if err != nil {
			return err
		}
		raw := p.RawCallback
		if raw == nil && p.CheckoutRequestID != nil {
			// Daraja may call back before the push job has recorded the checkout id; that callback was kept as an orphan.
			raw, err = q.OrphanCallback(ctx, *p.CheckoutRequestID)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		var cb daraja.STKCallback
		if raw != nil {
			cb, err = daraja.ParseSTKCallback(raw)
		}
		if raw == nil || err != nil || !cb.Paid() {
			if !expired {
				retry = s.cfg.VerifyAfter
				return nil
			}
			return s.settle(ctx, tx, p.UserID, paymentID, "unconfirmed", "paid_without_callback", nil, now)
		}
		used, err := q.ReceiptUsedElsewhere(ctx, store.ReceiptUsedElsewhereParams{MpesaReceipt: &cb.Receipt, ID: paymentID})
		if err != nil {
			return err
		}
		if used {
			return s.settle(ctx, tx, p.UserID, paymentID, "unconfirmed", "receipt_reused", nil, now)
		}
		return s.creditPayment(ctx, tx, creditInput{paymentID: paymentID, user: p.UserID, target: *p.Msisdn, amount: cb.AmountKES,
			receipt: cb.Receipt, payer: cb.MSISDN, now: now, actor: audit.Event{ActorType: audit.System}})
	})
	return retry, err
}

type creditInput struct {
	paymentID, user, target string
	amount                  int64
	receipt, payer          string
	now                     time.Time
	actor                   audit.Event // who credits: the system after a status query, or an operator from a statement
	// channel is "c2b" for a paybill payment, which proves no phone (anyone can pay into any account number);
	// otherwise an STK top-up, whose approved prompt proves the target.
	channel string
}

// creditPayment posts a confirmed top-up: the deposit net of excise, recovery of any debt the player owes, the payment
// marked credited, the phone proof (STK only) and the audit record, all in the caller's transaction.
func (s *Service) creditPayment(ctx context.Context, tx pgx.Tx, in creditInput) error {
	q := store.New(tx)
	rule, err := s.rules.ActiveRule(ctx, tx, exciseCode, in.now)
	if err != nil {
		return fmt.Errorf("payments: excise rule: %w", err)
	}
	posting, err := ledger.Deposit(in.paymentID, in.user, in.amount, rule, deductExcise)
	if err != nil {
		return err
	}
	if _, err := s.wallet.Apply(ctx, tx, posting); err != nil {
		return err
	}
	recovered, err := s.wallet.RecoverDebt(ctx, tx, in.user, in.paymentID)
	if err != nil {
		return err
	}
	if err := q.MarkCredited(ctx, store.MarkCreditedParams{AmountKes: &in.amount, Receipt: &in.receipt, At: in.now, ID: in.paymentID}); err != nil {
		return err
	}
	ev := in.actor
	ev.Action, ev.EntityType, ev.EntityID = "payment.topup.credited", "payment", in.paymentID
	ev.Details = map[string]any{"amount_kes": in.amount, "receipt": in.receipt, "debt_recovered_kes": recovered}
	if in.channel == "c2b" {
		ev.Action = "payment.c2b.credited"
	} else {
		ev.Details["phone_proof"] = s.provePhone(ctx, tx, in.user, in.target, in.payer)
	}
	return s.audit.Record(ctx, tx, ev)
}

// provePhone records that the player controls the push target: the prompt reached that phone and was approved with
// its M-Pesa PIN. A payer number that Daraja reports and that differs from the target is suspicious, so the phone is
// then not marked proved. Failures here never undo the credit: the money was paid. It runs in a savepoint so a refused
// verification (another account already proved the number) does not abort the transaction.
func (s *Service) provePhone(ctx context.Context, tx pgx.Tx, userID, target, reported string) string {
	if reported != "" && reported != target {
		return "payer_mismatch"
	}
	sp, err := tx.Begin(ctx)
	if err != nil {
		return "error"
	}
	if err := s.ids.MarkPhoneVerified(ctx, sp, userID, target); err != nil {
		_ = sp.Rollback(ctx)
		switch {
		case errors.Is(err, identity.ErrPhoneTaken):
			return "taken_by_another_account"
		case errors.Is(err, identity.ErrPhoneMismatch):
			return "number_changed"
		}
		return "error"
	}
	if err := sp.Commit(ctx); err != nil {
		return "error"
	}
	return "proved"
}

func (s *Service) settleTx(ctx context.Context, paymentID, status, reason string, code *int32) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		p, err := store.New(tx).LockPayment(ctx, paymentID)
		if err != nil {
			return err
		}
		if final(p.Status) {
			return nil
		}
		now, err := s.clk.Now(ctx, tx)
		if err != nil {
			return err
		}
		return s.settle(ctx, tx, p.UserID, paymentID, status, reason, code, now)
	})
}

func (s *Service) settle(ctx context.Context, tx pgx.Tx, userID, paymentID, status, reason string, code *int32, now time.Time) error {
	var r *string
	if reason != "" {
		r = &reason
	}
	if err := store.New(tx).SetPaymentOutcome(ctx, store.SetPaymentOutcomeParams{Status: status, FailureReason: r, ResultCode: code, At: now, ID: paymentID}); err != nil {
		return err
	}
	return s.audit.Record(ctx, tx, audit.Event{ActorType: audit.System, Action: "payment.topup." + status, EntityType: "payment", EntityID: paymentID,
		Details: map[string]any{"user_id": userID, "reason": reason}})
}

// Sweep enqueues verification for payments that have been pending longer than the verify delay, and for paybill
// confirmations whose verification has stalled, in case a job or a callback was lost.
func (s *Service) Sweep(ctx context.Context) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		now, err := s.clk.Now(ctx, tx)
		if err != nil {
			return err
		}
		q := store.New(tx)
		ids, err := q.StalePending(ctx, now.Add(-s.cfg.VerifyAfter))
		if err != nil {
			return err
		}
		for _, id := range ids {
			if err := s.enq.EnqueueVerify(ctx, tx, id, FromSweep); err != nil {
				return err
			}
		}
		if s.c2b == nil {
			return nil
		}
		before := now.Add(-s.c2b.cfg.StatusWait)
		stale, err := q.StaleC2B(ctx, &before)
		if err != nil {
			return err
		}
		for _, id := range stale {
			if err := s.enq.EnqueueC2B(ctx, tx, id, FromSweep); err != nil {
				return err
			}
		}
		return nil
	})
}
