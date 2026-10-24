package darajamock_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/itsivali/lottery-platform/internal/payments/daraja"
	"github.com/itsivali/lottery-platform/internal/payments/darajamock"
	"github.com/itsivali/lottery-platform/internal/platform/clock"
)

// receiver plays our API: it records what Daraja posts to each path and answers validation with verdict.
type receiver struct {
	mu      sync.Mutex
	got     map[string][][]byte
	verdict string
}

func (r *receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	raw, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	r.got[req.URL.Path] = append(r.got[req.URL.Path], raw)
	verdict := r.verdict
	r.mu.Unlock()
	if strings.HasSuffix(req.URL.Path, "/validate") {
		_, _ = w.Write([]byte(`{"ResultCode":"` + verdict + `","ResultDesc":"x"}`))
		return
	}
	_, _ = w.Write([]byte(`{"ResultCode":0,"ResultDesc":"Success"}`))
}

func (r *receiver) bodies(path string) [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.got[path]
}

var c2bStart = time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)

type c2bEnv struct {
	mock   *darajamock.Server
	client *daraja.Client
	recv   *receiver
	base   string
}

func newC2B(t *testing.T, validation bool) *c2bEnv {
	t.Helper()
	recv := &receiver{got: map[string][][]byte{}, verdict: "0"}
	api := httptest.NewServer(recv)
	t.Cleanup(api.Close)
	m := darajamock.New(darajamock.Options{ConsumerKey: "ck", ConsumerSecret: "cs", ShortCode: "174379", Passkey: "pk",
		C2BShortCode: "600982", Initiator: "testapi", SecurityCredential: "cred", ExternalValidation: validation, HTTPClient: api.Client()})
	ms := httptest.NewServer(m)
	t.Cleanup(ms.Close)
	c, err := daraja.New(daraja.Config{BaseURL: ms.URL, ConsumerKey: "ck", ConsumerSecret: "cs", ShortCode: "174379", Passkey: "pk",
		CallbackURL: api.URL + "/stk"}, ms.Client(), clock.NewManual(c2bStart))
	if err != nil {
		t.Fatal(err)
	}
	if c, err = c.WithC2B(daraja.C2BConfig{ShortCode: "600982", Initiator: "testapi", SecurityCredential: "cred",
		ResultURL: api.URL + "/status-result", TimeoutURL: api.URL + "/status-timeout"}); err != nil {
		t.Fatal(err)
	}
	if err := c.RegisterC2BURLs(context.Background(), api.URL+"/validate", api.URL+"/confirm", daraja.C2BCancelOnTimeout); err != nil {
		t.Fatal(err)
	}
	return &c2bEnv{mock: m, client: c, recv: recv, base: api.URL}
}

func TestC2B_RegisterRefusesForbiddenWordsAndOtherShortcodes(t *testing.T) {
	e := newC2B(t, false)
	ctx := context.Background()
	if err := e.client.RegisterC2BURLs(ctx, e.base+"/hooks/mpesa/validate", e.base+"/confirm", daraja.C2BCancelOnTimeout); !errors.Is(err, daraja.ErrRejected) {
		t.Fatalf("mpesa in url: %v", err)
	}
	if v, c := e.mock.RegisteredC2BURLs(); v != e.base+"/validate" || c != e.base+"/confirm" {
		t.Fatalf("registration changed: %s %s", v, c)
	}
}

func TestC2B_SimulatedPaymentIsConfirmedAndStatusCompletes(t *testing.T) {
	e := newC2B(t, false)
	ctx := context.Background()
	if err := e.client.SimulateC2B(ctx, 150, "254708374149", "0712345678"); err != nil {
		t.Fatal(err)
	}
	e.mock.Flush()
	conf := e.recv.bodies("/confirm")
	if len(conf) != 1 {
		t.Fatalf("confirmations = %d", len(conf))
	}
	p, err := daraja.ParseC2BNotification(conf[0])
	if err != nil {
		t.Fatal(err)
	}
	if p.AmountKES != 150 || p.BillRef != "0712345678" || p.MSISDN != "254708374149" || p.ShortCode != "600982" {
		t.Fatalf("p = %+v", p)
	}
	req, err := e.client.TransactionStatus(ctx, p.TransID)
	if err != nil {
		t.Fatal(err)
	}
	e.mock.Flush()
	res := e.recv.bodies("/status-result")
	if len(res) != 1 {
		t.Fatalf("results = %d", len(res))
	}
	r, err := daraja.ParseStatusResult(res[0])
	if err != nil {
		t.Fatal(err)
	}
	if !r.Completed() || r.Receipt != p.TransID || r.AmountKES != 150 || r.OriginatorConversationID != req.OriginatorConversationID ||
		!r.CreditedTo("600982") || r.Payer() != "254708374149" {
		t.Fatalf("r = %+v", r)
	}
	if got := e.mock.StatusRequests(); len(got) != 1 || got[0] != p.TransID {
		t.Fatalf("status requests = %v", got)
	}
}

func TestC2B_ValidationRefusalCancelsThePayment(t *testing.T) {
	e := newC2B(t, true)
	e.recv.verdict = "C2B00012"
	receipt, err := e.mock.Pay(context.Background(), 100, "254708374149", "nobody")
	if err != nil {
		t.Fatal(err)
	}
	if len(e.recv.bodies("/validate")) != 1 || len(e.recv.bodies("/confirm")) != 0 {
		t.Fatal("a refused payment was confirmed")
	}
	if pb := e.mock.PayBills(); len(pb) != 1 || !pb[0].Cancelled {
		t.Fatalf("paybills = %+v", pb)
	}
	if _, err := e.client.TransactionStatus(context.Background(), receipt); err != nil {
		t.Fatal(err)
	}
	e.mock.Flush()
	r, err := daraja.ParseStatusResult(e.recv.bodies("/status-result")[0])
	if err != nil || r.Completed() {
		t.Fatalf("a cancelled payment completed: %+v %v", r, err)
	}
}

func TestC2B_Faults(t *testing.T) {
	ctx := context.Background()
	t.Run("duplicate confirmation and masked phone", func(t *testing.T) {
		e := newC2B(t, false)
		e.mock.SetNextC2BFault(darajamock.C2BFault{DuplicateConfirmation: true, MaskPhone: true})
		if _, err := e.mock.Pay(ctx, 100, "254708374149", "x"); err != nil {
			t.Fatal(err)
		}
		conf := e.recv.bodies("/confirm")
		if len(conf) != 2 {
			t.Fatalf("confirmations = %d", len(conf))
		}
		if p, err := daraja.ParseC2BNotification(conf[0]); err != nil || p.MSISDN != "" {
			t.Fatalf("masked: %+v %v", p, err)
		}
	})
	t.Run("dropped confirmation", func(t *testing.T) {
		e := newC2B(t, false)
		e.mock.SetNextC2BFault(darajamock.C2BFault{DropConfirmation: true})
		if _, err := e.mock.Pay(ctx, 100, "254708374149", "x"); err != nil {
			t.Fatal(err)
		}
		if len(e.recv.bodies("/confirm")) != 0 {
			t.Fatal("confirmed")
		}
	})
	t.Run("status answers", func(t *testing.T) {
		for name, tc := range map[string]struct {
			fault darajamock.C2BFault
			path  string
			check func(daraja.StatusResult) bool
		}{
			"failure":   {darajamock.C2BFault{StatusResultCode: 2001}, "/status-result", func(r daraja.StatusResult) bool { return r.ResultCode == 2001 }},
			"amount":    {darajamock.C2BFault{StatusAmountKES: 90}, "/status-result", func(r daraja.StatusResult) bool { return r.Completed() && r.AmountKES == 90 }},
			"reversed":  {darajamock.C2BFault{StatusState: "Reversed"}, "/status-result", func(r daraja.StatusResult) bool { return !r.Completed() && r.Status == "Reversed" }},
			"payer":     {darajamock.C2BFault{StatusPayer: "254711111111"}, "/status-result", func(r daraja.StatusResult) bool { return r.Payer() == "254711111111" }},
			"timed out": {darajamock.C2BFault{StatusTimeout: true}, "/status-timeout", func(r daraja.StatusResult) bool { return r.ResultCode == 1037 }},
		} {
			e := newC2B(t, false)
			e.mock.SetNextC2BFault(tc.fault)
			receipt, err := e.mock.Pay(ctx, 100, "254708374149", "x")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := e.client.TransactionStatus(ctx, receipt); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			e.mock.Flush()
			got := e.recv.bodies(tc.path)
			if len(got) != 1 {
				t.Fatalf("%s: %d bodies at %s", name, len(got), tc.path)
			}
			r, err := daraja.ParseStatusResult(got[0])
			if err != nil || !tc.check(r) {
				t.Errorf("%s: %+v %v", name, r, err)
			}
		}
	})
	t.Run("status outage and silence", func(t *testing.T) {
		e := newC2B(t, false)
		e.mock.SetNextC2BFault(darajamock.C2BFault{StatusHTTP: 503})
		receipt, _ := e.mock.Pay(ctx, 100, "254708374149", "x")
		if _, err := e.client.TransactionStatus(ctx, receipt); !errors.Is(err, daraja.ErrUnavailable) {
			t.Fatalf("outage: %v", err)
		}
		e.mock.SetNextC2BFault(darajamock.C2BFault{StatusDrop: true})
		receipt, _ = e.mock.Pay(ctx, 100, "254708374149", "x")
		if _, err := e.client.TransactionStatus(ctx, receipt); err != nil {
			t.Fatal(err)
		}
		e.mock.Flush()
		if len(e.recv.bodies("/status-result")) != 0 {
			t.Fatal("a dropped status was answered")
		}
	})
	t.Run("wrong initiator", func(t *testing.T) {
		e := newC2B(t, false)
		m := httptest.NewServer(e.mock)
		t.Cleanup(m.Close)
		bad, _ := daraja.New(daraja.Config{BaseURL: m.URL, ConsumerKey: "ck", ConsumerSecret: "cs", ShortCode: "174379", Passkey: "pk", CallbackURL: "http://x/cb"}, m.Client(), clock.NewManual(c2bStart))
		bad, _ = bad.WithC2B(daraja.C2BConfig{ShortCode: "600982", Initiator: "someone", SecurityCredential: "cred", ResultURL: "http://x/r", TimeoutURL: "http://x/t"})
		if _, err := bad.TransactionStatus(ctx, "MCC0000001"); !errors.Is(err, daraja.ErrRejected) {
			t.Fatalf("wrong initiator: %v", err)
		}
	})
}
