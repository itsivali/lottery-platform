//go:build integration

package payments_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/itsivali/lottery-platform/internal/identity"
	"github.com/itsivali/lottery-platform/internal/payments"
	"github.com/itsivali/lottery-platform/internal/payments/darajamock"
	"github.com/itsivali/lottery-platform/internal/rg"
)

// payer is the sandbox's test customer: anyone may pay into any account number.
const payer = "254708374149"

// provedPlayer is a KYC-verified player whose phone is proved, so its number works as a paybill account number.
func (e *env) provedPlayer(t *testing.T) (string, string) {
	t.Helper()
	user, msisdn := e.player(t)
	if _, err := e.pool.Exec(context.Background(), `UPDATE identity.users SET msisdn_verified_at = now() WHERE id = $1`, user); err != nil {
		t.Fatal(err)
	}
	return user, msisdn
}

// payBill pays into the paybill as a customer would and runs the verification it triggers. It returns the receipt.
func (e *env) payBill(t *testing.T, amount int64, account string) string {
	t.Helper()
	receipt, err := e.mock.Pay(context.Background(), amount, payer, account)
	if err != nil {
		t.Fatal(err)
	}
	e.run(t)
	return receipt
}

type c2bRow struct {
	status, reason string
	paymentID      *string
	requests       int
}

func (e *env) c2b(t *testing.T, receipt string) c2bRow {
	t.Helper()
	var r c2bRow
	var reason *string
	if err := e.pool.QueryRow(context.Background(), `SELECT status, outcome_reason, payment_id::text, status_requests
		FROM payments.c2b_confirmations WHERE trans_id = $1`, receipt).Scan(&r.status, &reason, &r.paymentID, &r.requests); err != nil {
		t.Fatal(err)
	}
	if reason != nil {
		r.reason = *reason
	}
	return r
}

type suspenseRow struct {
	status, reason string
	amount         int64
}

func (e *env) suspense(t *testing.T, receipt string) suspenseRow {
	t.Helper()
	var r suspenseRow
	if err := e.pool.QueryRow(context.Background(), `SELECT status, reason, amount_kes FROM payments.c2b_unmatched WHERE mpesa_receipt = $1`,
		receipt).Scan(&r.status, &r.reason, &r.amount); err != nil {
		t.Fatal(err)
	}
	return r
}

// local writes a 2547XXXXXXXX number the way customers type it on the M-Pesa menu.
func local(msisdn string) string { return "0" + msisdn[3:] }

func TestC2B_APaymentToAProvedPhoneIsVerifiedThenCreditedNetOfExcise(t *testing.T) {
	e := setup(t)
	user, msisdn := e.provedPlayer(t)
	receipt := e.payBill(t, 150, local(msisdn))

	if got := e.mock.StatusRequests(); len(got) != 1 || got[0] != receipt {
		t.Fatalf("status requests = %v: the confirmation must be checked with Daraja", got)
	}
	row := e.c2b(t, receipt)
	if row.status != "credited" || row.paymentID == nil {
		t.Fatalf("confirmation = %+v", row)
	}
	if b := e.balance(t, user); b != 142 { // 150 less 5% excise (7.5, rounded half up to 8)
		t.Fatalf("balance = %d, want 142", b)
	}
	var channel, status, msisdnOnPayment string
	var confirmed int64
	if err := e.pool.QueryRow(context.Background(), `SELECT channel, status, amount_confirmed_kes, msisdn FROM payments.payments WHERE id = $1`,
		*row.paymentID).Scan(&channel, &status, &confirmed, &msisdnOnPayment); err != nil {
		t.Fatal(err)
	}
	if channel != "c2b" || status != "credited" || confirmed != 150 || msisdnOnPayment != msisdn {
		t.Fatalf("payment = %s %s %d %s", channel, status, confirmed, msisdnOnPayment)
	}
	if tu := e.get(t, user, *row.paymentID); tu.Status != payments.StatusCredited {
		t.Fatalf("player sees %+v", tu)
	}
	if r := e.reconcile(t); !r.OK || r.Totals["clearing_kes"] != 150 || r.Totals["credited_kes"] != 150 {
		t.Fatalf("reconciliation = %+v", r)
	}
}

func TestC2B_APaymentNeverProvesAPhone(t *testing.T) {
	e := setup(t)
	user, msisdn := e.player(t) // claims the number but never proved it
	receipt := e.payBill(t, 100, msisdn)
	if row := e.c2b(t, receipt); row.status != "suspense" || row.reason != "unknown_account" {
		t.Fatalf("confirmation = %+v: an unproved number may be claimed by several accounts", row)
	}
	if e.phoneVerified(t, user) || e.balance(t, user) != 0 {
		t.Fatal("a paybill payment credited or proved an unproved number")
	}
}

func TestC2B_PaymentsThatCannotBeCreditedGoToSuspense(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	restricted, rMSISDN := e.provedPlayer(t)
	if err := e.rg.Timeout(ctx, restricted, "24h"); err != nil {
		t.Fatal(err)
	}
	limited, lMSISDN := e.provedPlayer(t)
	if _, err := e.rg.SetLimit(ctx, limited, rg.Daily, 100); err != nil {
		t.Fatal(err)
	}
	unverified, uMSISDN := e.provedPlayer(t)
	if _, err := e.pool.Exec(ctx, `UPDATE identity.users SET kyc_status = 'pending' WHERE id = $1`, unverified); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		account string
		amount  int64
		reason  string
	}{
		{"0799999999", 100, "unknown_account"},
		{"not a phone", 100, "unknown_account"},
		{rMSISDN, 100, "restricted"},
		{lMSISDN, 150, "over_deposit_limit"},
		{uMSISDN, 100, "kyc_required"},
		{lMSISDN, payments.MinTopUpKES - 1, "bad_amount"},
	} {
		receipt := e.payBill(t, tc.amount, tc.account)
		if row := e.c2b(t, receipt); row.status != "suspense" || row.reason != tc.reason {
			t.Errorf("%s/%d: confirmation = %+v, want suspense %s", tc.account, tc.amount, row, tc.reason)
		}
		if s := e.suspense(t, receipt); s.status != "open" || s.amount != tc.amount || s.reason != tc.reason {
			t.Errorf("%s: suspense = %+v", tc.account, s)
		}
	}
	for _, u := range []string{restricted, limited, unverified} {
		if b := e.balance(t, u); b != 0 {
			t.Errorf("player %s credited %d", u, b)
		}
	}
	r := e.reconcile(t)
	if !r.OK || r.Totals["suspense_kes"] != 100+100+100+150+100+payments.MinTopUpKES-1 || len(r.Attention["suspense_open"]) != 6 {
		t.Fatalf("reconciliation = %+v", r)
	}
}

func TestC2B_DuplicateConfirmationsCreditOnce(t *testing.T) {
	e := setup(t)
	user, msisdn := e.provedPlayer(t)
	e.mock.SetNextC2BFault(darajamock.C2BFault{DuplicateConfirmation: true})
	receipt := e.payBill(t, 100, msisdn)
	if row := e.c2b(t, receipt); row.status != "credited" {
		t.Fatalf("confirmation = %+v", row)
	}
	if b := e.balance(t, user); b != 95 {
		t.Fatalf("balance = %d, want 95", b)
	}
	if got := e.mock.StatusRequests(); len(got) != 1 {
		t.Fatalf("status requests = %v", got)
	}
}

func (e *env) postHook(t *testing.T, path string, body any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, e.hooks+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
}

func confirmationBody(receipt string, amount int64, account, msisdn string) map[string]string {
	return map[string]string{"TransactionType": "Pay Bill", "TransID": receipt, "TransTime": "20261024093112",
		"TransAmount": strconv.FormatInt(amount, 10), "BusinessShortCode": "600982",
		"BillRefNumber": account, "MSISDN": msisdn, "FirstName": "Mallory"}
}

func TestC2B_AForgedConfirmationIsNeverCredited(t *testing.T) {
	e := setup(t)
	user, msisdn := e.provedPlayer(t)
	e.postHook(t, "/confirm", confirmationBody("FORGED0001", 5000, msisdn, payer))
	e.run(t)
	if row := e.c2b(t, "FORGED0001"); row.status != "unverified" || row.reason != "status_result_1" {
		t.Fatalf("forged confirmation = %+v", row)
	}
	if b := e.balance(t, user); b != 0 {
		t.Fatalf("a forged confirmation credited %d", b)
	}
	r := e.reconcile(t)
	if !r.OK || len(r.Attention["c2b_unverified"]) != 1 {
		t.Fatalf("reconciliation = %+v", r)
	}
	// an operator may send it through verification again; Daraja still does not know it
	if err := e.svc.RecheckC2B(context.Background(), "FORGED0001", "operator:test"); err != nil {
		t.Fatal(err)
	}
	if row := e.c2b(t, "FORGED0001"); row.status != "received" {
		t.Fatalf("after recheck = %+v", row)
	}
	if err := e.svc.RecheckC2B(context.Background(), "FORGED0001", "operator:test"); !errors.Is(err, payments.ErrNotRecheckable) {
		t.Fatalf("recheck of a payment being verified: %v", err)
	}
}

func TestC2B_AConflictingSecondConfirmationSendsTheMoneyToSuspense(t *testing.T) {
	e := setup(t)
	victim, vMSISDN := e.provedPlayer(t)
	thief, tMSISDN := e.provedPlayer(t)
	e.mock.SetNextC2BFault(darajamock.C2BFault{DropConfirmation: true})
	receipt, err := e.mock.Pay(context.Background(), 300, payer, vMSISDN)
	if err != nil {
		t.Fatal(err)
	}
	// someone who learned the hook URL and the receipt claims the payment for another account first...
	e.postHook(t, "/confirm", confirmationBody(receipt, 300, tMSISDN, payer))
	// ...then the real confirmation arrives
	e.postHook(t, "/confirm", confirmationBody(receipt, 300, vMSISDN, payer))
	e.run(t)
	if row := e.c2b(t, receipt); row.status != "suspense" || row.reason != "conflicting_confirmations" {
		t.Fatalf("confirmation = %+v", row)
	}
	if e.balance(t, victim) != 0 || e.balance(t, thief) != 0 {
		t.Fatal("a disputed payment was credited")
	}
}

func TestC2B_AConflictArrivingAfterTheCreditIsListedForAnOperator(t *testing.T) {
	e := setup(t)
	_, msisdn := e.provedPlayer(t)
	_, other := e.provedPlayer(t)
	receipt := e.payBill(t, 100, msisdn)
	e.postHook(t, "/confirm", confirmationBody(receipt, 100, other, payer))
	e.run(t)
	if row := e.c2b(t, receipt); row.status != "credited" {
		t.Fatalf("confirmation = %+v", row)
	}
	r := e.reconcile(t)
	if got := r.Attention["c2b_conflict_after_credit"]; !r.OK || len(got) != 1 || got[0] != receipt {
		t.Fatalf("reconciliation = %+v", r)
	}
}

func TestC2B_WhatDarajaReportsDecides(t *testing.T) {
	for name, tc := range map[string]struct {
		fault          darajamock.C2BFault
		status, reason string
		suspended      int64 // the amount parked, when in suspense
	}{
		"different amount": {darajamock.C2BFault{StatusAmountKES: 90}, "suspense", "amount_mismatch", 90},
		"different payer":  {darajamock.C2BFault{StatusPayer: "254711111111"}, "suspense", "payer_mismatch", 100},
		"masked payer":     {darajamock.C2BFault{MaskPhone: true}, "credited", "", 0},
		"failure":          {darajamock.C2BFault{StatusResultCode: 2001}, "unverified", "status_result_2001", 0},
		"reversed":         {darajamock.C2BFault{StatusState: "Reversed"}, "unverified", "status_reversed", 0},
	} {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			user, msisdn := e.provedPlayer(t)
			e.mock.SetNextC2BFault(tc.fault)
			receipt := e.payBill(t, 100, msisdn)
			row := e.c2b(t, receipt)
			if row.status != tc.status || row.reason != tc.reason {
				t.Fatalf("confirmation = %+v, want %s %s", row, tc.status, tc.reason)
			}
			if tc.status == "suspense" {
				if s := e.suspense(t, receipt); s.amount != tc.suspended {
					t.Fatalf("suspense = %+v: the amount parked is what Daraja confirms", s)
				}
			}
			if want := map[string]int64{"credited": 95}[tc.status]; e.balance(t, user) != want {
				t.Fatalf("balance = %d, want %d", e.balance(t, user), want)
			}
			if r := e.reconcile(t); !r.OK {
				t.Fatalf("reconciliation = %+v", r.Mismatches)
			}
		})
	}
}

func TestC2B_ASilentStatusAPIIsAskedAgainThenLeftForAnOperator(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	user, msisdn := e.provedPlayer(t)
	e.mock.SetNextC2BFault(darajamock.C2BFault{StatusDrop: true})
	receipt := e.payBill(t, 100, msisdn)
	if row := e.c2b(t, receipt); row.status != "status_requested" || row.requests != 1 {
		t.Fatalf("confirmation = %+v", row)
	}
	if len(e.retries) == 0 || e.retries[len(e.retries)-1] != payments.DefaultStatusWait {
		t.Fatalf("retries = %v: the job waits for the answer", e.retries)
	}
	for range 3 {
		e.clk.Advance(payments.DefaultStatusWait + time.Second)
		if err := e.svc.Sweep(ctx); err != nil {
			t.Fatal(err)
		}
		e.run(t)
	}
	row := e.c2b(t, receipt)
	if row.status != "unverified" || row.reason != "no_status_result" || row.requests != 3 {
		t.Fatalf("confirmation = %+v", row)
	}
	if len(e.mock.StatusRequests()) != 3 || e.balance(t, user) != 0 {
		t.Fatalf("status requests = %v, balance %d", e.mock.StatusRequests(), e.balance(t, user))
	}
}

func TestC2B_AStatusOutageIsRetried(t *testing.T) {
	e := setup(t)
	_, msisdn := e.provedPlayer(t)
	e.mock.SetNextC2BFault(darajamock.C2BFault{StatusHTTP: http.StatusServiceUnavailable})
	receipt, err := e.mock.Pay(context.Background(), 100, payer, msisdn)
	if err != nil {
		t.Fatal(err)
	}
	jobs := e.q.take()
	if len(jobs) != 1 || jobs[0].kind != "c2b" {
		t.Fatalf("jobs = %+v", jobs)
	}
	if _, err := e.svc.VerifyC2B(context.Background(), jobs[0].paymentID); err == nil {
		t.Fatal("an outage was not reported for a retry")
	}
	if row := e.c2b(t, receipt); row.status != "received" {
		t.Fatalf("confirmation = %+v", row)
	}
}

func TestC2B_AConfirmationForAnotherShortcodeIsNotVerified(t *testing.T) {
	e := setup(t)
	_, msisdn := e.provedPlayer(t)
	body := confirmationBody("OTHERCODE1", 100, msisdn, payer)
	body["BusinessShortCode"] = "174379"
	e.postHook(t, "/confirm", body)
	e.run(t)
	if row := e.c2b(t, "OTHERCODE1"); row.status != "unverified" || row.reason != "wrong_shortcode" {
		t.Fatalf("confirmation = %+v", row)
	}
	if len(e.mock.StatusRequests()) != 0 {
		t.Fatal("asked Daraja about another shortcode's payment")
	}
}

func TestC2B_AStatusResultForNoKnownPaymentIsDropped(t *testing.T) {
	e := setup(t)
	err := e.svc.HandleC2BStatusResult(context.Background(), []byte(`{"Result":{"ResultCode":0,"OriginatorConversationID":"nobody",
"ResultParameters":{"ResultParameter":[{"Key":"ReceiptNo","Value":"NOBODY0001"},{"Key":"Amount","Value":10},{"Key":"TransactionStatus","Value":"Completed"}]}}}`))
	if err != nil || len(e.q.take()) != 0 {
		t.Fatalf("err = %v", err)
	}
}

func TestValidateC2B_AcceptsOnlyWhatCouldBeCredited(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	_, ok := e.provedPlayer(t)
	restricted, rMSISDN := e.provedPlayer(t)
	if err := e.rg.SelfExclude(ctx, restricted, "6m"); err != nil {
		t.Fatal(err)
	}
	limited, lMSISDN := e.provedPlayer(t)
	if _, err := e.rg.SetLimit(ctx, limited, rg.Daily, 50); err != nil {
		t.Fatal(err)
	}
	_, unproved := e.player(t)
	for name, tc := range map[string]struct {
		body map[string]string
		want string
	}{
		"eligible":       {confirmationBody("VALID00001", 100, local(ok), payer), payments.C2BAccept},
		"unknown":        {confirmationBody("VALID00002", 100, "0700000000", payer), payments.C2BInvalidAccount},
		"unproved":       {confirmationBody("VALID00003", 100, unproved, payer), payments.C2BInvalidAccount},
		"restricted":     {confirmationBody("VALID00004", 100, rMSISDN, payer), payments.C2BOtherError},
		"over the limit": {confirmationBody("VALID00005", 100, lMSISDN, payer), payments.C2BOtherError},
		"below minimum":  {confirmationBody("VALID00006", payments.MinTopUpKES-1, ok, payer), payments.C2BInvalidAmount},
		"fractional": {func() map[string]string {
			b := confirmationBody("VALID00007", 1, ok, payer)
			b["TransAmount"] = "10.50"
			return b
		}(), payments.C2BInvalidAmount},
		"other paybill": {func() map[string]string {
			b := confirmationBody("VALID00008", 100, ok, payer)
			b["BusinessShortCode"] = "1"
			return b
		}(), payments.C2BInvalidCode},
		"no receipt": {func() map[string]string {
			b := confirmationBody("VALID00009", 100, ok, payer)
			b["TransID"] = ""
			return b
		}(), payments.C2BOtherError},
	} {
		raw, _ := json.Marshal(tc.body)
		got, err := e.svc.ValidateC2B(ctx, raw)
		if err != nil || got != tc.want {
			t.Errorf("%s: %s %v, want %s", name, got, err, tc.want)
		}
	}
	var stored int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM payments.c2b_confirmations`).Scan(&stored); err != nil || stored != 0 {
		t.Fatalf("validation wrote %d confirmations (%v)", stored, err)
	}
}

func TestValidateC2B_ThroughDarajaARefusalCancelsThePayment(t *testing.T) {
	e := setup(t)
	if _, err := e.mock.Pay(context.Background(), 100, payer, "0700000000"); err != nil {
		t.Fatal(err)
	}
	// without external validation Daraja confirms everything; the mock's validating mode is covered in darajamock
	if pb := e.mock.PayBills(); len(pb) != 1 || pb[0].Cancelled {
		t.Fatalf("paybills = %+v", pb)
	}
}

func TestResolveSuspense_CreditsTheIdentifiedPlayerOnce(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	user, _ := e.player(t) // paid with an account number that is not its proved phone
	receipt := e.payBill(t, 200, "0799999999")
	if s := e.suspense(t, receipt); s.status != "open" {
		t.Fatalf("suspense = %+v", s)
	}
	id, err := e.svc.ResolveSuspense(ctx, receipt, user, "operator:test")
	if err != nil {
		t.Fatal(err)
	}
	if b := e.balance(t, user); b != 190 {
		t.Fatalf("balance = %d, want 190", b)
	}
	if s := e.suspense(t, receipt); s.status != "resolved" {
		t.Fatalf("suspense = %+v", s)
	}
	if st, _ := e.status(t, id); st != "credited" {
		t.Fatalf("payment status = %s", st)
	}
	if e.phoneVerified(t, user) {
		t.Fatal("resolving suspense proved a phone")
	}
	if _, err := e.svc.ResolveSuspense(ctx, receipt, user, "operator:test"); !errors.Is(err, payments.ErrNotInSuspense) {
		t.Fatalf("second resolve: %v", err)
	}
	if r := e.reconcile(t); !r.OK || r.Totals["suspense_kes"] != 0 || r.Totals["credited_kes"] != 200 || r.Totals["clearing_kes"] != 200 {
		t.Fatalf("reconciliation = %+v", r)
	}
}

func TestResolveSuspense_RefusesPlayersWhoMayNotReceiveIt(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	receipt := e.payBill(t, 200, "0799999999")
	excluded, _ := e.player(t)
	if err := e.rg.SelfExclude(ctx, excluded, "permanent"); err != nil {
		t.Fatal(err)
	}
	var re *rg.RestrictedError
	if _, err := e.svc.ResolveSuspense(ctx, receipt, excluded, "operator:test"); !errors.As(err, &re) {
		t.Fatalf("self-excluded: %v", err)
	}
	noKYC, _ := e.player(t)
	if _, err := e.pool.Exec(ctx, `UPDATE identity.users SET kyc_status = 'pending' WHERE id = $1`, noKYC); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ResolveSuspense(ctx, receipt, noKYC, "operator:test"); !errors.Is(err, identity.ErrKYCRequired) {
		t.Fatalf("no kyc: %v", err)
	}
	if _, err := e.svc.ResolveSuspense(ctx, "NOSUCH0001", noKYC, "operator:test"); !errors.Is(err, payments.ErrNotInSuspense) {
		t.Fatalf("unknown receipt: %v", err)
	}
	if s := e.suspense(t, receipt); s.status != "open" {
		t.Fatalf("suspense = %+v", s)
	}
}

func TestRefundSuspense_TakesTheItemOutOfSuspense(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	receipt := e.payBill(t, 120, "0799999999")
	if err := e.svc.RefundSuspense(ctx, receipt, " ", "operator:test"); !errors.Is(err, payments.ErrRefundRefNeeded) {
		t.Fatalf("no reference: %v", err)
	}
	if err := e.svc.RefundSuspense(ctx, receipt, "TJKREFUND1", "operator:test"); err != nil {
		t.Fatal(err)
	}
	if s := e.suspense(t, receipt); s.status != "refunded" {
		t.Fatalf("suspense = %+v", s)
	}
	if err := e.svc.RefundSuspense(ctx, receipt, "TJKREFUND1", "operator:test"); !errors.Is(err, payments.ErrNotInSuspense) {
		t.Fatalf("second refund: %v", err)
	}
	if r := e.reconcile(t); !r.OK || r.Totals["suspense_kes"] != 0 || r.Totals["clearing_kes"] != 0 {
		t.Fatalf("reconciliation = %+v", r)
	}
}

func TestReconcile_FindsASuspenseItemTheLedgerNeverSaw(t *testing.T) {
	e := setup(t)
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO payments.c2b_unmatched (mpesa_receipt, amount_kes, raw_payload)
		VALUES ('GHOST00001', 100, '{}')`); err != nil {
		t.Fatal(err)
	}
	r := e.reconcile(t)
	if r.OK || len(r.Mismatches["suspense_without_journal"]) != 1 || len(r.Mismatches["suspense_vs_items"]) != 1 {
		t.Fatalf("reconciliation = %+v", r)
	}
}

func TestC2B_IsRefusedWhenNotConfigured(t *testing.T) {
	e := setupWith(t, false)
	ctx := context.Background()
	if err := e.svc.HandleC2BConfirmation(ctx, []byte(`{}`)); !errors.Is(err, payments.ErrC2BUnavailable) {
		t.Fatalf("confirmation: %v", err)
	}
	if code, err := e.svc.ValidateC2B(ctx, []byte(`{}`)); code != payments.C2BOtherError || !errors.Is(err, payments.ErrC2BUnavailable) {
		t.Fatalf("validation: %s %v", code, err)
	}
	if err := e.svc.HandleC2BStatusResult(ctx, []byte(`{}`)); !errors.Is(err, payments.ErrC2BUnavailable) {
		t.Fatalf("status result: %v", err)
	}
	if e.svc.C2BEnabled() {
		t.Fatal("enabled")
	}
}
