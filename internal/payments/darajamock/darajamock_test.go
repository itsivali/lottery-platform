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

type callbacks struct {
	mu     sync.Mutex
	bodies []daraja.STKCallback
	status int
}

func (c *callbacks) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		cb, err := daraja.ParseSTKCallback(raw)
		if err != nil {
			t.Errorf("mock sent a callback the real parser refuses: %v: %s", err, raw)
		}
		c.mu.Lock()
		c.bodies = append(c.bodies, cb)
		st := c.status
		c.mu.Unlock()
		if st != 0 {
			w.WriteHeader(st)
		}
	})
}

func (c *callbacks) got() []daraja.STKCallback {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]daraja.STKCallback(nil), c.bodies...)
}

type env struct {
	mock   *darajamock.Server
	client *daraja.Client
	cbs    *callbacks
}

func setup(t *testing.T) env {
	t.Helper()
	cbs := &callbacks{}
	cbSrv := httptest.NewServer(cbs.handler(t))
	t.Cleanup(cbSrv.Close)
	m := darajamock.New(darajamock.Options{ConsumerKey: "ck", ConsumerSecret: "cs", ShortCode: "174379", Passkey: "pk", HTTPClient: cbSrv.Client()})
	srv := httptest.NewServer(m)
	t.Cleanup(srv.Close)
	c, err := daraja.New(daraja.Config{BaseURL: srv.URL, ConsumerKey: "ck", ConsumerSecret: "cs", ShortCode: "174379",
		Passkey: "pk", CallbackURL: cbSrv.URL + "/cb"}, srv.Client(), clock.NewManual(time.Date(2026, 10, 22, 6, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatal(err)
	}
	return env{mock: m, client: c, cbs: cbs}
}

func push(t *testing.T, e env, amount int64) daraja.STKPushResult {
	t.Helper()
	res, err := e.client.STKPush(context.Background(), daraja.STKPushRequest{MSISDN: "254712345678", AmountKES: amount, AccountRef: "TOPUP"})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestMock_AHappyPushIsPendingUntilCompletedThenCallsBackAndQueriesAsPaid(t *testing.T) {
	e := setup(t)
	res := push(t, e, 150)

	st, err := e.client.STKQuery(context.Background(), res.CheckoutRequestID)
	if err != nil || st.State != daraja.Pending {
		t.Fatalf("before completion: %+v %v", st, err)
	}
	if err := e.mock.Complete(context.Background(), res.CheckoutRequestID); err != nil {
		t.Fatal(err)
	}
	got := e.cbs.got()
	if len(got) != 1 || !got[0].Paid() || got[0].AmountKES != 150 || got[0].MSISDN != "254712345678" ||
		got[0].Receipt == "" || got[0].CheckoutRequestID != res.CheckoutRequestID {
		t.Fatalf("callbacks = %+v", got)
	}
	st, err = e.client.STKQuery(context.Background(), res.CheckoutRequestID)
	if err != nil || st.State != daraja.Paid {
		t.Fatalf("after completion: %+v %v", st, err)
	}
	if p := e.mock.Pushes(); len(p) != 1 || p[0].AmountKES != 150 || p[0].MSISDN != "254712345678" {
		t.Fatalf("pushes = %+v", p)
	}
}

func TestMock_PendingCanBeReportedTheWayTheSandboxDoes(t *testing.T) {
	e := setup(t)
	e.mock.SetNextFault(darajamock.Fault{PendingAs4999: true})
	res := push(t, e, 10)
	st, err := e.client.STKQuery(context.Background(), res.CheckoutRequestID)
	if err != nil || st.State != daraja.Pending || st.ResultCode != 4999 {
		t.Fatalf("before completion: %+v %v", st, err)
	}
	if err := e.mock.Complete(context.Background(), res.CheckoutRequestID); err != nil {
		t.Fatal(err)
	}
	if st, err := e.client.STKQuery(context.Background(), res.CheckoutRequestID); err != nil || st.State != daraja.Paid {
		t.Fatalf("after completion: %+v %v", st, err)
	}
}

func TestMock_ReceiptsAreUnique(t *testing.T) {
	e := setup(t)
	a, b := push(t, e, 10), push(t, e, 10)
	for _, id := range []string{a.CheckoutRequestID, b.CheckoutRequestID} {
		if err := e.mock.Complete(context.Background(), id); err != nil {
			t.Fatal(err)
		}
	}
	got := e.cbs.got()
	if len(got) != 2 || got[0].Receipt == got[1].Receipt || a.CheckoutRequestID == b.CheckoutRequestID {
		t.Fatalf("callbacks = %+v", got)
	}
}

func TestMock_FaultsShapeTheNextPush(t *testing.T) {
	cases := map[string]struct {
		f     darajamock.Fault
		check func(t *testing.T, e env, cbs []daraja.STKCallback, st daraja.STKStatus)
	}{
		"customer cancels": {darajamock.Fault{ResultCode: 1032}, func(t *testing.T, _ env, cbs []daraja.STKCallback, st daraja.STKStatus) {
			if len(cbs) != 1 || cbs[0].Paid() || cbs[0].ResultCode != 1032 || st.State != daraja.Failed || st.ResultCode != 1032 {
				t.Fatalf("cbs=%+v st=%+v", cbs, st)
			}
		}},
		"callback lost": {darajamock.Fault{DropCallback: true}, func(t *testing.T, _ env, cbs []daraja.STKCallback, st daraja.STKStatus) {
			if len(cbs) != 0 || st.State != daraja.Paid {
				t.Fatalf("cbs=%+v st=%+v", cbs, st)
			}
		}},
		"callback duplicated": {darajamock.Fault{DuplicateCallback: true}, func(t *testing.T, _ env, cbs []daraja.STKCallback, _ daraja.STKStatus) {
			if len(cbs) != 2 || cbs[0] != cbs[1] {
				t.Fatalf("cbs=%+v", cbs)
			}
		}},
		"customer paid a different amount": {darajamock.Fault{PaidAmountKES: 7}, func(t *testing.T, _ env, cbs []daraja.STKCallback, _ daraja.STKStatus) {
			if len(cbs) != 1 || cbs[0].AmountKES != 7 {
				t.Fatalf("cbs=%+v", cbs)
			}
		}},
		"phone masked": {darajamock.Fault{MaskPhone: true}, func(t *testing.T, _ env, cbs []daraja.STKCallback, _ daraja.STKStatus) {
			if len(cbs) != 1 || cbs[0].MSISDN != "" || !cbs[0].Paid() {
				t.Fatalf("cbs=%+v", cbs)
			}
		}},
		"query outage": {darajamock.Fault{QueryStatus: http.StatusServiceUnavailable}, nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			e.mock.SetNextFault(tc.f)
			res := push(t, e, 100)
			if err := e.mock.Complete(context.Background(), res.CheckoutRequestID); err != nil {
				t.Fatal(err)
			}
			st, err := e.client.STKQuery(context.Background(), res.CheckoutRequestID)
			if tc.check == nil {
				if !errors.Is(err, daraja.ErrUnavailable) {
					t.Fatalf("query err = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			tc.check(t, e, e.cbs.got(), st)
		})
	}
}

func TestMock_PushFaultsAndTheFaultIsUsedOnlyOnce(t *testing.T) {
	e := setup(t)
	e.mock.SetNextFault(darajamock.Fault{PushStatus: http.StatusInternalServerError})
	_, err := e.client.STKPush(context.Background(), daraja.STKPushRequest{MSISDN: "254712345678", AmountKES: 10, AccountRef: "A"})
	if !errors.Is(err, daraja.ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
	push(t, e, 10) // the next one is clean
	e.mock.SetNextFault(darajamock.Fault{PushStatus: http.StatusBadRequest})
	if _, err := e.client.STKPush(context.Background(), daraja.STKPushRequest{MSISDN: "254712345678", AmountKES: 10, AccountRef: "A"}); !errors.Is(err, daraja.ErrRejected) {
		t.Fatalf("err = %v", err)
	}
}

func TestMock_RefusesWrongCredentialsAndWrongPasswords(t *testing.T) {
	m := darajamock.New(darajamock.Options{ConsumerKey: "ck", ConsumerSecret: "cs", ShortCode: "174379", Passkey: "pk"})
	srv := httptest.NewServer(m)
	defer srv.Close()
	cfg := daraja.Config{BaseURL: srv.URL, ConsumerKey: "ck", ConsumerSecret: "wrong", ShortCode: "174379", Passkey: "pk", CallbackURL: "http://x/cb"}
	c, _ := daraja.New(cfg, srv.Client(), clock.NewManual(time.Now()))
	if _, err := c.STKPush(context.Background(), daraja.STKPushRequest{MSISDN: "254712345678", AmountKES: 10, AccountRef: "A"}); !errors.Is(err, daraja.ErrUnavailable) {
		t.Fatalf("bad secret: %v", err)
	}
	cfg.ConsumerSecret, cfg.Passkey = "cs", "not-the-passkey"
	c, _ = daraja.New(cfg, srv.Client(), clock.NewManual(time.Now()))
	if _, err := c.STKPush(context.Background(), daraja.STKPushRequest{MSISDN: "254712345678", AmountKES: 10, AccountRef: "A"}); !errors.Is(err, daraja.ErrRejected) {
		t.Fatalf("bad password: %v", err)
	}
	if err := m.Complete(context.Background(), "nope"); err == nil {
		t.Fatal("completed an unknown checkout")
	}
}

func TestMock_ControlEndpointSetsFaultsAndCompletes(t *testing.T) {
	e := setup(t)
	srv := httptest.NewServer(e.mock)
	defer srv.Close()
	resp, err := post(t, srv, "/__mock/fault", `{"result_code":2001}`)
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("fault: %v %v", resp, err)
	}
	_ = resp.Body.Close()
	res := push(t, e, 10)
	resp, err = post(t, srv, "/__mock/complete/"+res.CheckoutRequestID, "")
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("complete: %v %v", resp, err)
	}
	_ = resp.Body.Close()
	if got := e.cbs.got(); len(got) != 1 || got[0].ResultCode != 2001 {
		t.Fatalf("callbacks = %+v", got)
	}
	resp, err = post(t, srv, "/__mock/fault", `not json`)
	if err != nil || resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad fault: %v %v", resp, err)
	}
	_ = resp.Body.Close()
}

func TestMock_AutoCompleteCallsBackWithoutBeingAsked(t *testing.T) {
	e := setup(t)
	e.mock.AutoComplete(context.Background(), 0)
	res := push(t, e, 25)
	deadline := time.Now().Add(5 * time.Second)
	for len(e.cbs.got()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no callback")
		}
		time.Sleep(5 * time.Millisecond) //nolint:forbidigo // polling helper for an asynchronous callback
	}
	if got := e.cbs.got(); got[0].CheckoutRequestID != res.CheckoutRequestID || got[0].AmountKES != 25 {
		t.Fatalf("callbacks = %+v", got)
	}
}

func TestMock_ACallbackTheReceiverRefusesIsReported(t *testing.T) {
	e := setup(t)
	e.cbs.status = http.StatusInternalServerError
	res := push(t, e, 10)
	if err := e.mock.Complete(context.Background(), res.CheckoutRequestID); err == nil {
		t.Fatal("a refused callback was reported as delivered")
	}
}

func post(t *testing.T, srv *httptest.Server, path, body string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return srv.Client().Do(req)
}
