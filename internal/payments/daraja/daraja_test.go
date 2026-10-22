package daraja_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/itsivali/lottery-platform/internal/payments/daraja"
	"github.com/itsivali/lottery-platform/internal/platform/clock"
)

// 09:30:05 in Nairobi is 06:30:05 UTC; Daraja timestamps are local (EAT) time.
var at = time.Date(2026, 10, 22, 6, 30, 5, 0, time.UTC)

func testConfig(base string) daraja.Config {
	return daraja.Config{
		BaseURL: base, ConsumerKey: "ck", ConsumerSecret: "cs", ShortCode: "174379",
		Passkey: "passkey-secret", CallbackURL: "https://api.example/v1/payments/mpesa/stk/tok",
	}
}

type fakeDaraja struct {
	tokens   atomic.Int32
	push     func(w http.ResponseWriter, body map[string]any)
	query    func(w http.ResponseWriter, body map[string]any)
	lastAuth atomic.Value
}

func (f *fakeDaraja) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /oauth/v1/generate", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("grant_type") != "client_credentials" {
			t.Errorf("grant_type = %q", r.URL.Query().Get("grant_type"))
		}
		user, pass, ok := r.BasicAuth()
		if !ok || user != "ck" || pass != "cs" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		n := f.tokens.Add(1)
		_, _ = w.Write([]byte(`{"access_token":"tok-` + string(rune('0'+n)) + `","expires_in":"3599"}`))
	})
	decode := func(r *http.Request) map[string]any {
		f.lastAuth.Store(r.Header.Get("Authorization"))
		var m map[string]any
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
			t.Errorf("decode: %v", err)
		}
		return m
	}
	mux.HandleFunc("POST /mpesa/stkpush/v1/processrequest", func(w http.ResponseWriter, r *http.Request) { f.push(w, decode(r)) })
	mux.HandleFunc("POST /mpesa/stkpushquery/v1/query", func(w http.ResponseWriter, r *http.Request) { f.query(w, decode(r)) })
	return mux
}

func newClient(t *testing.T, f *fakeDaraja) (*daraja.Client, *clock.Manual) {
	t.Helper()
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	clk := clock.NewManual(at)
	c, err := daraja.New(testConfig(srv.URL), srv.Client(), clk)
	if err != nil {
		t.Fatal(err)
	}
	return c, clk
}

func accepted(w http.ResponseWriter, _ map[string]any) {
	_, _ = w.Write([]byte(`{"MerchantRequestID":"m-1","CheckoutRequestID":"ws_CO_1","ResponseCode":"0","ResponseDescription":"Success. Request accepted for processing","CustomerMessage":"Success"}`))
}

func TestSTKPush_SendsASignedPaybillRequestAndReturnsTheCheckoutID(t *testing.T) {
	var got map[string]any
	f := &fakeDaraja{push: func(w http.ResponseWriter, b map[string]any) { got = b; accepted(w, b) }}
	c, _ := newClient(t, f)

	res, err := c.STKPush(context.Background(), daraja.STKPushRequest{MSISDN: "254712345678", AmountKES: 150, AccountRef: "TOPUP", Description: "Wallet top-up"})
	if err != nil {
		t.Fatal(err)
	}
	if res.MerchantRequestID != "m-1" || res.CheckoutRequestID != "ws_CO_1" {
		t.Fatalf("res = %+v", res)
	}
	wantPassword := base64.StdEncoding.EncodeToString([]byte("174379" + "passkey-secret" + "20261022093005"))
	want := map[string]any{
		"BusinessShortCode": "174379", "Password": wantPassword, "Timestamp": "20261022093005",
		"TransactionType": "CustomerPayBillOnline", "Amount": float64(150), "PartyA": "254712345678", "PartyB": "174379",
		"PhoneNumber": "254712345678", "CallBackURL": "https://api.example/v1/payments/mpesa/stk/tok",
		"AccountReference": "TOPUP", "TransactionDesc": "Wallet top-up",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
	if f.lastAuth.Load() != "Bearer tok-1" {
		t.Errorf("auth = %v", f.lastAuth.Load())
	}
}

func TestSTKPush_RefusesBadInputWithoutCallingDaraja(t *testing.T) {
	f := &fakeDaraja{push: func(http.ResponseWriter, map[string]any) { t.Error("daraja was called") }}
	c, _ := newClient(t, f)
	for name, req := range map[string]daraja.STKPushRequest{
		"zero amount":     {MSISDN: "254712345678", AmountKES: 0, AccountRef: "A"},
		"negative amount": {MSISDN: "254712345678", AmountKES: -5, AccountRef: "A"},
		"huge amount":     {MSISDN: "254712345678", AmountKES: 250_001, AccountRef: "A"},
		"bad msisdn":      {MSISDN: "0712345678", AmountKES: 10, AccountRef: "A"},
		"long reference":  {MSISDN: "254712345678", AmountKES: 10, AccountRef: "THIRTEEN-CHAR"},
		"empty reference": {MSISDN: "254712345678", AmountKES: 10},
	} {
		if _, err := c.STKPush(context.Background(), req); !errors.Is(err, daraja.ErrInvalidRequest) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestSTKPush_ReusesTheTokenUntilShortlyBeforeItExpires(t *testing.T) {
	f := &fakeDaraja{push: accepted}
	c, clk := newClient(t, f)
	req := daraja.STKPushRequest{MSISDN: "254712345678", AmountKES: 10, AccountRef: "A"}
	for range 3 {
		if _, err := c.STKPush(context.Background(), req); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.tokens.Load(); n != 1 {
		t.Fatalf("token fetched %d times, want 1", n)
	}
	clk.Advance(3599*time.Second - 61*time.Second) // still more than a minute left
	if _, err := c.STKPush(context.Background(), req); err != nil || f.tokens.Load() != 1 {
		t.Fatalf("err=%v tokens=%d, want the cached token", err, f.tokens.Load())
	}
	clk.Advance(2 * time.Second) // under a minute left: refresh rather than risk an expired token mid-request
	if _, err := c.STKPush(context.Background(), req); err != nil || f.tokens.Load() != 2 {
		t.Fatalf("err=%v tokens=%d, want a fresh token", err, f.tokens.Load())
	}
	if f.lastAuth.Load() != "Bearer tok-2" {
		t.Fatalf("auth = %v", f.lastAuth.Load())
	}
}

func TestSTKPush_AnInvalidTokenAnswerIsRetriedOnceWithAFreshToken(t *testing.T) {
	var calls atomic.Int32
	f := &fakeDaraja{}
	f.push = func(w http.ResponseWriter, b map[string]any) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"requestId":"r","errorCode":"404.001.03","errorMessage":"Invalid Access Token"}`))
			return
		}
		accepted(w, b)
	}
	c, _ := newClient(t, f)
	if _, err := c.STKPush(context.Background(), daraja.STKPushRequest{MSISDN: "254712345678", AmountKES: 10, AccountRef: "A"}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || f.tokens.Load() != 2 {
		t.Fatalf("calls=%d tokens=%d", calls.Load(), f.tokens.Load())
	}
}

func TestSTKPush_RejectionsAndOutagesAreDistinguishedAndNeverLeakSecrets(t *testing.T) {
	cases := map[string]struct {
		h    func(http.ResponseWriter, map[string]any)
		want error
	}{
		"rejected 400": {func(w http.ResponseWriter, _ map[string]any) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"requestId":"r","errorCode":"400.002.02","errorMessage":"Bad Request - Invalid PhoneNumber"}`))
		}, daraja.ErrRejected},
		"non-zero response code": {func(w http.ResponseWriter, _ map[string]any) {
			_, _ = w.Write([]byte(`{"MerchantRequestID":"m","CheckoutRequestID":"c","ResponseCode":"1","ResponseDescription":"no"}`))
		}, daraja.ErrRejected},
		"accepted without checkout id": {func(w http.ResponseWriter, _ map[string]any) {
			_, _ = w.Write([]byte(`{"ResponseCode":"0"}`))
		}, daraja.ErrUnavailable},
		"server error": {func(w http.ResponseWriter, _ map[string]any) { w.WriteHeader(http.StatusInternalServerError) }, daraja.ErrUnavailable},
		"throttled":    {func(w http.ResponseWriter, _ map[string]any) { w.WriteHeader(http.StatusTooManyRequests) }, daraja.ErrUnavailable},
		"garbage":      {func(w http.ResponseWriter, _ map[string]any) { _, _ = w.Write([]byte("<html>")) }, daraja.ErrUnavailable},
	}
	for name, tc := range cases {
		c, _ := newClient(t, &fakeDaraja{push: tc.h})
		_, err := c.STKPush(context.Background(), daraja.STKPushRequest{MSISDN: "254712345678", AmountKES: 10, AccountRef: "A"})
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
			continue
		}
		for _, secret := range []string{"passkey-secret", "cs", "tok-1", "254712345678"} {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("%s: error %q leaks %q", name, err, secret)
			}
		}
	}
}

func TestSTKPush_TokenFailureIsAnOutage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	defer srv.Close()
	c, err := daraja.New(testConfig(srv.URL), srv.Client(), clock.NewManual(at))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.STKPush(context.Background(), daraja.STKPushRequest{MSISDN: "254712345678", AmountKES: 10, AccountRef: "A"}); !errors.Is(err, daraja.ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
}

func TestSTKQuery_MapsTheResultCode(t *testing.T) {
	var got map[string]any
	cases := map[string]struct {
		body   string
		status int
		want   daraja.STKStatus
		err    error
	}{
		"paid": {`{"ResponseCode":"0","ResultCode":"0","ResultDesc":"The service request is processed successfully."}`, 200,
			daraja.STKStatus{State: daraja.Paid, ResultCode: 0, ResultDesc: "The service request is processed successfully."}, nil},
		"cancelled by user": {`{"ResponseCode":"0","ResultCode":"1032","ResultDesc":"Request cancelled by user"}`, 200,
			daraja.STKStatus{State: daraja.Failed, ResultCode: 1032, ResultDesc: "Request cancelled by user"}, nil},
		"numeric result code": {`{"ResponseCode":"0","ResultCode":1037,"ResultDesc":"DS timeout user cannot be reached"}`, 200,
			daraja.STKStatus{State: daraja.Failed, ResultCode: 1037, ResultDesc: "DS timeout user cannot be reached"}, nil},
		// seen live from the sandbox on 2026-10-22: HTTP 200 with a result code that only means "not finished yet"
		"still under processing": {`{"ResponseCode":"0","ResultCode":"4999","ResultDesc":"The transaction is still under processing"}`, 200,
			daraja.STKStatus{State: daraja.Pending, ResultCode: 4999, ResultDesc: "The transaction is still under processing"}, nil},
		"still processing": {`{"requestId":"r","errorCode":"500.001.1001","errorMessage":"The transaction is being processed"}`, 500,
			daraja.STKStatus{State: daraja.Pending}, nil},
		"unknown checkout": {`{"requestId":"r","errorCode":"400.002.02","errorMessage":"Bad Request - Invalid CheckoutRequestID"}`, 400,
			daraja.STKStatus{}, daraja.ErrRejected},
		"outage":         {`oops`, 503, daraja.STKStatus{}, daraja.ErrUnavailable},
		"no result code": {`{"ResponseCode":"0"}`, 200, daraja.STKStatus{}, daraja.ErrUnavailable},
	}
	for name, tc := range cases {
		f := &fakeDaraja{query: func(w http.ResponseWriter, b map[string]any) {
			got = b
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}}
		c, _ := newClient(t, f)
		st, err := c.STKQuery(context.Background(), "ws_CO_1")
		if !errors.Is(err, tc.err) || st != tc.want {
			t.Errorf("%s: got %+v, %v; want %+v, %v", name, st, err, tc.want, tc.err)
		}
		if got["CheckoutRequestID"] != "ws_CO_1" || got["BusinessShortCode"] != "174379" || got["Timestamp"] != "20261022093005" {
			t.Errorf("%s: request = %v", name, got)
		}
	}
}

func TestNew_RefusesIncompleteOrProductionConfig(t *testing.T) {
	ok := testConfig("https://sandbox.safaricom.co.ke")
	if _, err := daraja.New(ok, http.DefaultClient, clock.NewManual(at)); err != nil {
		t.Fatalf("sandbox: %v", err)
	}
	for name, mut := range map[string]func(*daraja.Config){
		"no key":          func(c *daraja.Config) { c.ConsumerKey = "" },
		"no secret":       func(c *daraja.Config) { c.ConsumerSecret = "" },
		"no shortcode":    func(c *daraja.Config) { c.ShortCode = "" },
		"no passkey":      func(c *daraja.Config) { c.Passkey = "" },
		"no callback":     func(c *daraja.Config) { c.CallbackURL = "" },
		"http callback":   func(c *daraja.Config) { c.CallbackURL = "http://api.example/cb" }, // Safaricom only calls https
		"relative base":   func(c *daraja.Config) { c.BaseURL = "sandbox.safaricom.co.ke" },
		"bad base":        func(c *daraja.Config) { c.BaseURL = "::" },
		"production base": func(c *daraja.Config) { c.BaseURL = "https://api.safaricom.co.ke" },
	} {
		c := ok
		mut(&c)
		if _, err := daraja.New(c, http.DefaultClient, clock.NewManual(at)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	local := ok
	local.BaseURL, local.CallbackURL = "http://daraja-mock:8080", "http://api:8080/v1/payments/mpesa/stk/tok"
	if _, err := daraja.New(local, http.DefaultClient, clock.NewManual(at)); err != nil {
		t.Errorf("local mock over http: %v", err)
	}
	prod := ok
	prod.BaseURL, prod.AllowProduction = "https://api.safaricom.co.ke", true
	if _, err := daraja.New(prod, http.DefaultClient, clock.NewManual(at)); err != nil {
		t.Errorf("explicitly allowed production: %v", err)
	}
}

func TestSTKQuery_AResultCodeOutsideTheInt32RangeIsAnOutageNotAnOutcome(t *testing.T) {
	f := &fakeDaraja{query: func(w http.ResponseWriter, _ map[string]any) {
		_, _ = w.Write([]byte(`{"ResponseCode":"0","ResultCode":"99999999999","ResultDesc":"?"}`))
	}}
	c, _ := newClient(t, f)
	if _, err := c.STKQuery(context.Background(), "ws_CO_1"); !errors.Is(err, daraja.ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
}
