package payments

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/itsivali/lottery-platform/internal/audit"
	"github.com/itsivali/lottery-platform/internal/identity"
	"github.com/itsivali/lottery-platform/internal/ledger"
	"github.com/itsivali/lottery-platform/internal/payments/daraja"
	"github.com/itsivali/lottery-platform/internal/payments/store"
	"github.com/itsivali/lottery-platform/internal/rg"
)

// Paybill (C2B) payments. A customer pays into the paybill from the M-Pesa menu and types an account number: the
// player's registered phone number. Daraja may first ask our validation URL whether to accept the payment, then posts
// a confirmation. The confirmation is a claim (ADR 0006): it is stored, verified with the Transaction Status API, and
// only then credited, through the same credit path as STK top-ups, to the account that has proved that phone. Anything
// that cannot be credited safely (no such account, a restricted player, a disagreement between the confirmation and
// Daraja's answer) is parked in suspense for an operator. A paybill payment never proves a phone: anyone can pay into
// any account number.

// Errors of the paybill operator actions.
var (
	ErrC2BUnavailable  = errors.New("payments: paybill payments are not configured")
	ErrNotInSuspense   = errors.New("payments: no open suspense item for this receipt")
	ErrNotRecheckable  = errors.New("payments: only an unverified paybill payment can be checked again")
	ErrRefundRefNeeded = errors.New("payments: the M-Pesa reference of the refund is required")
)

// Validation answers. Daraja shows the customer a generic failure for any refusal, so the codes stay coarse and never
// say why a known account was refused.
const (
	C2BAccept         = "0"
	C2BInvalidAccount = "C2B00012"
	C2BInvalidAmount  = "C2B00013"
	C2BInvalidCode    = "C2B00015"
	C2BOtherError     = "C2B00016"
)

// Defaults of the Transaction Status loop.
const (
	DefaultStatusWait        = time.Minute // how long to wait for a status answer before asking again
	DefaultMaxStatusRequests = 10
)

// C2BDaraja is the part of the Daraja client that verifies paybill payments.
type C2BDaraja interface {
	TransactionStatus(ctx context.Context, receipt string) (daraja.StatusRequest, error)
}

// C2BConfig configures paybill payments; zero durations and counts take the defaults.
type C2BConfig struct {
	ShortCode         string // the paybill customers pay into
	StatusWait        time.Duration
	MaxStatusRequests int
}

type c2bSetup struct {
	daraja C2BDaraja
	cfg    C2BConfig
}

// WithC2B turns paybill payments on. Without it the paybill hooks are not served and confirmations are refused.
func (s *Service) WithC2B(d C2BDaraja, cfg C2BConfig) *Service {
	if cfg.StatusWait <= 0 {
		cfg.StatusWait = DefaultStatusWait
	}
	if cfg.MaxStatusRequests <= 0 {
		cfg.MaxStatusRequests = DefaultMaxStatusRequests
	}
	s.c2b = &c2bSetup{daraja: d, cfg: cfg}
	return s
}

// C2BEnabled reports whether paybill payments are configured.
func (s *Service) C2BEnabled() bool { return s.c2b != nil }

// ValidateC2B answers Daraja's validation request for a paybill payment that has not happened yet: accept it only for
// the right paybill, a whole amount within bounds and an account number that is an eligible player's proved phone,
// within their deposit limits. It writes nothing. It returns a Daraja result code.
func (s *Service) ValidateC2B(ctx context.Context, raw []byte) (string, error) {
	if s.c2b == nil {
		return C2BOtherError, ErrC2BUnavailable
	}
	p, err := daraja.ParseC2BNotification(raw)
	switch {
	case errors.Is(err, daraja.ErrBadAmount):
		return C2BInvalidAmount, nil
	case err != nil:
		return C2BOtherError, nil
	case p.ShortCode != s.c2b.cfg.ShortCode:
		return C2BInvalidCode, nil
	}
	var code string
	err = pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		now, err := s.clk.Now(ctx, tx)
		if err != nil {
			return err
		}
		m, err := s.matchC2B(ctx, tx, p.BillRef, p.AmountKES, now)
		if err != nil {
			return err
		}
		switch m.reason {
		case "":
			code = C2BAccept
		case "unknown_account", "kyc_required", "account_suspended":
			code = C2BInvalidAccount
		case "bad_amount":
			code = C2BInvalidAmount
		default:
			code = C2BOtherError
		}
		return nil
	})
	if err != nil {
		return C2BOtherError, err
	}
	return code, nil
}

type c2bMatch struct {
	user, msisdn string
	reason       string // why the payment cannot be credited; "" when it can
}

// matchC2B finds the account a paybill payment is for and checks it may receive the amount now. A refusal comes back
// as a reason, not an error; errors are failures to check.
func (s *Service) matchC2B(ctx context.Context, tx pgx.Tx, billRef string, amount int64, now time.Time) (c2bMatch, error) {
	msisdn, err := identity.NormalizeMSISDN(billRef)
	if err != nil {
		return c2bMatch{reason: "unknown_account"}, nil
	}
	user, err := s.ids.ProvedPhoneOwner(ctx, tx, msisdn)
	if errors.Is(err, identity.ErrNoProvedOwner) {
		return c2bMatch{reason: "unknown_account"}, nil
	}
	if err != nil {
		return c2bMatch{}, err
	}
	m := c2bMatch{user: user, msisdn: msisdn}
	switch _, err := s.ids.AssertCanTopUp(ctx, tx, user); {
	case errors.Is(err, identity.ErrKYCRequired):
		m.reason = "kyc_required"
		return m, nil
	case errors.Is(err, identity.ErrAccountSuspended):
		m.reason = "account_suspended"
		return m, nil
	case err != nil:
		return c2bMatch{}, err
	}
	var re *rg.RestrictedError
	if err := s.rg.AssertNotRestricted(ctx, tx, user); errors.As(err, &re) {
		m.reason = "restricted"
		return m, nil
	} else if err != nil {
		return c2bMatch{}, err
	}
	if amount < MinTopUpKES || amount > daraja.MaxSTKAmountKES {
		m.reason = "bad_amount"
		return m, nil
	}
	var le *rg.LimitError
	if err := s.rg.AssertDepositAllowed(ctx, tx, user, amount, func(since time.Time) (int64, error) {
		return store.New(tx).DepositedSince(ctx, store.DepositedSinceParams{UserID: user, CreatedAt: since})
	}); errors.As(err, &le) {
		m.reason = "over_deposit_limit"
		return m, nil
	} else if err != nil {
		return c2bMatch{}, err
	}
	return m, nil
}

// HandleC2BConfirmation stores a paybill confirmation and enqueues its verification. It moves no money. A repeat for
// a receipt already stored is ignored, unless it disagrees with the first one: then the payment is marked conflicting
// and, once Daraja confirms it, goes to suspense instead of being credited to either account. A malformed body returns
// daraja.ErrBadCallback.
func (s *Service) HandleC2BConfirmation(ctx context.Context, raw []byte) error {
	if s.c2b == nil {
		return ErrC2BUnavailable
	}
	p, err := daraja.ParseC2BNotification(raw)
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		now, err := s.clk.Now(ctx, tx)
		if err != nil {
			return err
		}
		var payer *string
		if p.MSISDN != "" {
			payer = &p.MSISDN
		}
		id, err := q.InsertC2BConfirmation(ctx, store.InsertC2BConfirmationParams{TransID: p.TransID, Shortcode: p.ShortCode,
			BillRef: p.BillRef, PayerMsisdn: payer, AmountKes: p.AmountKES, Raw: raw, At: now})
		if errors.Is(err, pgx.ErrNoRows) {
			prev, err := q.LockC2BByTransID(ctx, p.TransID)
			if err != nil {
				return err
			}
			if prev.Shortcode == p.ShortCode && prev.BillRef == p.BillRef && prev.AmountKes == p.AmountKES && equalPtr(prev.PayerMsisdn, payer) {
				return nil
			}
			slog.WarnContext(ctx, "conflicting paybill confirmations for one receipt", "receipt", p.TransID, "status", prev.Status)
			if err := q.MarkC2BConflicting(ctx, store.MarkC2BConflictingParams{At: now, ID: prev.ID}); err != nil {
				return err
			}
			return s.audit.Record(ctx, tx, audit.Event{ActorType: audit.System, Action: "payment.c2b.conflict", EntityType: "c2b_confirmation",
				EntityID: prev.ID, Details: map[string]any{"receipt": p.TransID, "status": prev.Status}})
		}
		if err != nil {
			return err
		}
		if err := s.enq.EnqueueC2B(ctx, tx, id, AfterCallback); err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, audit.Event{ActorType: audit.System, Action: "payment.c2b.received", EntityType: "c2b_confirmation",
			EntityID: id, Details: map[string]any{"receipt": p.TransID, "amount_kes": p.AmountKES}})
	})
}

func equalPtr(a, b *string) bool { return (a == nil && b == nil) || (a != nil && b != nil && *a == *b) }

// HandleC2BStatusResult stores Daraja's Transaction Status answer and enqueues the settlement. The first answer for a
// payment counts; later ones are ignored. An answer that matches no payment is logged and dropped (the verification
// asks again). A malformed body returns daraja.ErrBadCallback.
func (s *Service) HandleC2BStatusResult(ctx context.Context, raw []byte) error {
	if s.c2b == nil {
		return ErrC2BUnavailable
	}
	r, err := daraja.ParseStatusResult(raw)
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		receipt := r.Receipt
		if receipt == "" {
			receipt = r.Occasion
		}
		row, err := q.LockC2BForResult(ctx, store.LockC2BForResultParams{Originator: &r.OriginatorConversationID, Receipt: receipt})
		if errors.Is(err, pgx.ErrNoRows) {
			slog.WarnContext(ctx, "transaction status result for no known paybill payment", "result_code", r.ResultCode)
			return nil
		}
		if err != nil {
			return err
		}
		if row.Status != "received" && row.Status != "status_requested" {
			return nil
		}
		now, err := s.clk.Now(ctx, tx)
		if err != nil {
			return err
		}
		if err := q.RecordC2BStatusResult(ctx, store.RecordC2BStatusResultParams{Result: raw, At: now, ID: row.ID}); err != nil {
			return err
		}
		return s.enq.EnqueueC2B(ctx, tx, row.ID, AfterStatus)
	})
}

// HandleC2BStatusTimeout notes that Daraja's queue gave up on a status request. Verification asks again on its own
// schedule, so this only logs.
func (s *Service) HandleC2BStatusTimeout(ctx context.Context, raw []byte) {
	r, err := daraja.ParseStatusResult(raw)
	if err != nil {
		slog.WarnContext(ctx, "unreadable transaction status timeout")
		return
	}
	slog.InfoContext(ctx, "transaction status request timed out at Daraja", "originator", r.OriginatorConversationID)
}

// VerifyC2B moves a paybill confirmation forward: it asks Daraja for the transaction's status, waits for the answer,
// and settles from it. It returns a positive duration when the job should look again after it; an error is an outage
// to retry with backoff.
func (s *Service) VerifyC2B(ctx context.Context, id string) (time.Duration, error) {
	c, err := store.New(s.pool).GetC2B(ctx, id)
	if err != nil {
		return 0, err
	}
	switch c.Status {
	case "status_received":
		return 0, s.settleC2B(ctx, id)
	case "received", "status_requested":
	default:
		return 0, nil
	}
	if s.c2b == nil {
		return 0, ErrC2BUnavailable
	}
	now, err := s.clk.Now(ctx, s.pool)
	if err != nil {
		return 0, err
	}
	cfg := s.c2b.cfg
	if c.Status == "status_requested" && c.StatusRequestedAt != nil {
		if wait := c.StatusRequestedAt.Add(cfg.StatusWait).Sub(now); wait > 0 {
			return wait, nil
		}
	}
	switch {
	case c.Shortcode != cfg.ShortCode:
		return 0, s.c2bOutcomeTx(ctx, id, "unverified", "wrong_shortcode")
	case int(c.StatusRequests) >= cfg.MaxStatusRequests:
		return 0, s.c2bOutcomeTx(ctx, id, "unverified", "no_status_result")
	}
	req, err := s.c2b.daraja.TransactionStatus(ctx, c.TransID)
	switch {
	case errors.Is(err, daraja.ErrRejected), errors.Is(err, daraja.ErrInvalidRequest):
		// a refused request is a configuration problem (initiator, credential) more often than a bad receipt
		return 0, s.c2bOutcomeTx(ctx, id, "unverified", "status_request_rejected")
	case err != nil:
		return 0, err
	}
	if _, err := store.New(s.pool).MarkC2BStatusRequested(ctx, store.MarkC2BStatusRequestedParams{At: &now, Originator: &req.OriginatorConversationID, ID: id}); err != nil {
		return 0, err
	}
	return cfg.StatusWait, nil
}

// settleC2B decides a paybill payment from Daraja's stored status answer: credit it, park it in suspense, or leave it
// unverified (no money confirmed) for an operator.
func (s *Service) settleC2B(ctx context.Context, id string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		c, err := q.LockC2B(ctx, id)
		if err != nil {
			return err
		}
		if c.Status != "status_received" {
			return nil
		}
		now, err := s.clk.Now(ctx, tx)
		if err != nil {
			return err
		}
		r, err := daraja.ParseStatusResult(c.StatusResult)
		if err != nil {
			return s.c2bOutcome(ctx, tx, c, "unverified", "unreadable_status_result", now)
		}
		switch {
		case !r.Completed() && r.ResultCode == 0:
			return s.c2bOutcome(ctx, tx, c, "unverified", "status_"+strings.ToLower(strings.ReplaceAll(r.Status, " ", "_")), now)
		case !r.Completed():
			return s.c2bOutcome(ctx, tx, c, "unverified", fmt.Sprint("status_result_", r.ResultCode), now)
		case r.Receipt != c.TransID:
			return s.c2bOutcome(ctx, tx, c, "unverified", "receipt_mismatch", now)
		case !r.CreditedTo(c.Shortcode):
			return s.c2bOutcome(ctx, tx, c, "unverified", "credited_elsewhere", now)
		}
		// Daraja confirms the money arrived; its amount is the one that counts.
		amount := r.AmountKES
		used, err := q.ReceiptCredited(ctx, &c.TransID)
		if err != nil {
			return err
		}
		switch {
		case used:
			return s.c2bOutcome(ctx, tx, c, "unverified", "receipt_reused", now)
		case c.Conflicting:
			return s.suspend(ctx, tx, c, amount, "conflicting_confirmations", now)
		case amount != c.AmountKes:
			return s.suspend(ctx, tx, c, amount, "amount_mismatch", now)
		case c.PayerMsisdn != nil && r.Payer() != "" && r.Payer() != *c.PayerMsisdn:
			return s.suspend(ctx, tx, c, amount, "payer_mismatch", now)
		}
		m, err := s.matchC2B(ctx, tx, c.BillRef, amount, now)
		if err != nil {
			return err
		}
		if m.reason != "" {
			return s.suspend(ctx, tx, c, amount, m.reason, now)
		}
		paymentID, err := q.InsertC2BPayment(ctx, store.InsertC2BPaymentParams{UserID: m.user, AmountKes: amount, Msisdn: &m.msisdn,
			IdempotencyKey: "c2b:" + c.TransID, At: now})
		if err != nil {
			return err
		}
		if err := s.creditPayment(ctx, tx, creditInput{paymentID: paymentID, user: m.user, target: m.msisdn, amount: amount,
			receipt: c.TransID, now: now, channel: "c2b", actor: audit.Event{ActorType: audit.System}}); err != nil {
			return err
		}
		return q.SetC2BOutcome(ctx, store.SetC2BOutcomeParams{Status: "credited", PaymentID: &paymentID, At: now, ID: c.ID})
	})
}

// suspend parks a confirmed paybill payment in suspense: an item for the operator and the money moved from
// MPESA_CLEARING to SUSPENSE, keyed on the item.
func (s *Service) suspend(ctx context.Context, tx pgx.Tx, c store.PaymentsC2bConfirmation, amount int64, reason string, now time.Time) error {
	q := store.New(tx)
	item, err := q.InsertUnmatched(ctx, store.InsertUnmatchedParams{Receipt: c.TransID, Msisdn: c.PayerMsisdn, AccountRef: &c.BillRef,
		AmountKes: amount, Raw: c.RawConfirmation, Reason: reason, At: now})
	if err != nil {
		return err
	}
	posting, err := ledger.C2BSuspense(item, amount)
	if err != nil {
		return err
	}
	if _, err := s.rules.Post(ctx, tx, posting.Journal); err != nil {
		return err
	}
	if err := q.SetC2BOutcome(ctx, store.SetC2BOutcomeParams{Status: "suspense", Reason: &reason, UnmatchedID: &item, At: now, ID: c.ID}); err != nil {
		return err
	}
	return s.audit.Record(ctx, tx, audit.Event{ActorType: audit.System, Action: "payment.c2b.suspense", EntityType: "c2b_unmatched",
		EntityID: item, Details: map[string]any{"receipt": c.TransID, "amount_kes": amount, "reason": reason}})
}

func (s *Service) c2bOutcomeTx(ctx context.Context, id, status, reason string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		c, err := store.New(tx).LockC2B(ctx, id)
		if err != nil {
			return err
		}
		if c.Status != "received" && c.Status != "status_requested" {
			return nil
		}
		now, err := s.clk.Now(ctx, tx)
		if err != nil {
			return err
		}
		return s.c2bOutcome(ctx, tx, c, status, reason, now)
	})
}

func (s *Service) c2bOutcome(ctx context.Context, tx pgx.Tx, c store.PaymentsC2bConfirmation, status, reason string, now time.Time) error {
	if err := store.New(tx).SetC2BOutcome(ctx, store.SetC2BOutcomeParams{Status: status, Reason: &reason, At: now, ID: c.ID}); err != nil {
		return err
	}
	return s.audit.Record(ctx, tx, audit.Event{ActorType: audit.System, Action: "payment.c2b." + status, EntityType: "c2b_confirmation",
		EntityID: c.ID, Details: map[string]any{"receipt": c.TransID, "reason": reason}})
}

// ResolveSuspense credits a suspense item to the player an operator has identified (for example the customer called
// and proved who they are). The player must be allowed to receive it now: an active, KYC-verified account that is
// not restricted and within its deposit limits; otherwise the money is refunded instead. It goes through the same
// credit path as every deposit (excise, debt recovery, receipt used once) and returns the new payment's id.
func (s *Service) ResolveSuspense(ctx context.Context, receipt, userID, operator string) (string, error) {
	var paymentID string
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		item, err := q.LockUnmatchedByReceipt(ctx, receipt)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && item.Status != "open") {
			return ErrNotInSuspense
		}
		if err != nil {
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
		if err := s.rg.AssertDepositAllowed(ctx, tx, userID, item.AmountKes, func(since time.Time) (int64, error) {
			return q.DepositedSince(ctx, store.DepositedSinceParams{UserID: userID, CreatedAt: since})
		}); err != nil {
			return err
		}
		used, err := q.ReceiptCredited(ctx, &receipt)
		if err != nil {
			return err
		}
		if used {
			return ErrReceiptUsed
		}
		release, err := ledger.C2BSuspenseRelease(item.ID, item.AmountKes)
		if err != nil {
			return err
		}
		if _, err := s.rules.Post(ctx, tx, release.Journal); err != nil {
			return err
		}
		if paymentID, err = q.InsertC2BPayment(ctx, store.InsertC2BPaymentParams{UserID: userID, AmountKes: item.AmountKes, Msisdn: &msisdn,
			IdempotencyKey: "c2b:" + receipt, At: now}); err != nil {
			return err
		}
		if err := s.creditPayment(ctx, tx, creditInput{paymentID: paymentID, user: userID, target: msisdn, amount: item.AmountKes,
			receipt: receipt, now: now, channel: "c2b", actor: audit.Event{ActorType: audit.Staff, ActorID: operator}}); err != nil {
			return err
		}
		return q.ResolveUnmatched(ctx, store.ResolveUnmatchedParams{PaymentID: &paymentID, At: &now, Operator: &operator, ID: item.ID})
	})
	return paymentID, err
}

// RefundSuspense records that a suspense item was sent back to the payer (through the M-Pesa portal or a B2C payment,
// whose reference the operator gives) and takes it out of suspense.
func (s *Service) RefundSuspense(ctx context.Context, receipt, refundRef, operator string) error {
	if strings.TrimSpace(refundRef) == "" {
		return ErrRefundRefNeeded
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		item, err := q.LockUnmatchedByReceipt(ctx, receipt)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && item.Status != "open") {
			return ErrNotInSuspense
		}
		if err != nil {
			return err
		}
		now, err := s.clk.Now(ctx, tx)
		if err != nil {
			return err
		}
		refund, err := ledger.C2BSuspenseRefund(item.ID, item.AmountKes)
		if err != nil {
			return err
		}
		if _, err := s.rules.Post(ctx, tx, refund.Journal); err != nil {
			return err
		}
		if err := q.RefundUnmatched(ctx, store.RefundUnmatchedParams{RefundRef: &refundRef, At: &now, Operator: &operator, ID: item.ID}); err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, audit.Event{ActorType: audit.Staff, ActorID: operator, Action: "payment.c2b.refunded", EntityType: "c2b_unmatched",
			EntityID: item.ID, Details: map[string]any{"receipt": receipt, "amount_kes": item.AmountKes, "refund_ref": refundRef}})
	})
}

// RecheckC2B sends an unverified paybill payment through verification again (after a configuration fix, or when
// Daraja was wrong). The sweep picks it up.
func (s *Service) RecheckC2B(ctx context.Context, receipt, operator string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		now, err := s.clk.Now(ctx, tx)
		if err != nil {
			return err
		}
		n, err := store.New(tx).RecheckC2B(ctx, store.RecheckC2BParams{At: now, TransID: receipt})
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotRecheckable
		}
		return s.audit.Record(ctx, tx, audit.Event{ActorType: audit.Staff, ActorID: operator, Action: "payment.c2b.recheck", EntityType: "c2b_confirmation",
			EntityID: receipt, Details: map[string]any{"receipt": receipt}})
	})
}
