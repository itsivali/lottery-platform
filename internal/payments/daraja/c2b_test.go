package daraja_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/itsivali/lottery-platform/internal/payments/daraja"
	"github.com/itsivali/lottery-platform/internal/platform/clock"
)

var c2bConfig = daraja.C2BConfig{
	ShortCode: "600982", Initiator: "testapi", SecurityCredential: "c2VjcmV0",
	ResultURL: "https://api.example/hooks/c2b/tok/status-result", TimeoutURL: "https://api.example/hooks/c2b/tok/status-timeout",
}

// c2bServer answers OAuth and hands every other request's path and body to h.
func c2bServer(t *testing.T, h func(w http.ResponseWriter, path string, body map[string]any)) *daraja.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/v1/generate" {
			_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":"3599"}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode: %v", err)
		}
		h(w, r.URL.Path, body)
	}))
	t.Cleanup(srv.Close)
	c, err := daraja.New(testConfig(srv.URL), srv.Client(), clock.NewManual(at))
	if err != nil {
		t.Fatal(err)
	}
	if c, err = c.WithC2B(c2bConfig); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestWithC2B_RefusesIncompleteConfiguration(t *testing.T) {
	c, err := daraja.New(testConfig("https://sandbox.safaricom.co.ke"), http.DefaultClient, clock.NewManual(at))
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*daraja.C2BConfig){
		"no shortcode":       func(c *daraja.C2BConfig) { c.ShortCode = "" },
		"no initiator":       func(c *daraja.C2BConfig) { c.Initiator = "" },
		"no credential":      func(c *daraja.C2BConfig) { c.SecurityCredential = "" },
		"relative result":    func(c *daraja.C2BConfig) { c.ResultURL = "/hooks/c2b/x/status-result" },
		"http to safaricom":  func(c *daraja.C2BConfig) { c.TimeoutURL = "http://api.example/t" },
		"missing timeout ur": func(c *daraja.C2BConfig) { c.TimeoutURL = "" },
	} {
		cfg := c2bConfig
		mutate(&cfg)
		if _, err := c.WithC2B(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestC2BCalls_AreRefusedWithoutC2BConfiguration(t *testing.T) {
	c, err := daraja.New(testConfig("https://sandbox.safaricom.co.ke"), http.DefaultClient, clock.NewManual(at))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := c.RegisterC2BURLs(ctx, "https://a/v", "https://a/c", daraja.C2BCancelOnTimeout); !errors.Is(err, daraja.ErrInvalidRequest) {
		t.Errorf("register: %v", err)
	}
	if _, err := c.TransactionStatus(ctx, "TJK1AB2CD3"); !errors.Is(err, daraja.ErrInvalidRequest) {
		t.Errorf("status: %v", err)
	}
}

func TestRegisterC2BURLs_SendsTheShortcodeURLsAndResponseType(t *testing.T) {
	var path string
	var got map[string]any
	c := c2bServer(t, func(w http.ResponseWriter, p string, body map[string]any) {
		path, got = p, body
		_, _ = w.Write([]byte(`{"OriginatorCoversationID":"o-1","ResponseCode":"0","ResponseDescription":"success"}`))
	})
	if err := c.RegisterC2BURLs(context.Background(), "https://a/hooks/c2b/s/validate", "https://a/hooks/c2b/s/confirm", daraja.C2BCancelOnTimeout); err != nil {
		t.Fatal(err)
	}
	if path != "/mpesa/c2b/v1/registerurl" || got["ShortCode"] != "600982" || got["ResponseType"] != "Cancelled" ||
		got["ValidationURL"] != "https://a/hooks/c2b/s/validate" || got["ConfirmationURL"] != "https://a/hooks/c2b/s/confirm" {
		t.Fatalf("path %s body %v", path, got)
	}
	if err := c.RegisterC2BURLs(context.Background(), "https://a/v", "https://a/c", "Maybe"); !errors.Is(err, daraja.ErrInvalidRequest) {
		t.Fatalf("bad response type: %v", err)
	}
}

func TestRegisterC2BURLs_AcceptsTheSandboxsZeroPaddedSuccessCode(t *testing.T) {
	c := c2bServer(t, func(w http.ResponseWriter, _ string, _ map[string]any) {
		_, _ = w.Write([]byte(`{"OriginatorCoversationID":"b65b-47a4","ResponseCode":"00000000","ResponseDescription":"Success"}`))
	})
	if err := c.RegisterC2BURLs(context.Background(), "https://a/v", "https://a/c", daraja.C2BCancelOnTimeout); err != nil {
		t.Fatalf("the sandbox's success answer was refused: %v", err)
	}
}

func TestRegisterC2BURLs_ClassifiesRefusalsAndOutages(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   error
	}{
		{400, `{"requestId":"r","errorCode":"400.003.02","errorMessage":"Bad Request - Invalid ValidationURL"}`, daraja.ErrRejected},
		{500, `{"requestId":"r","errorCode":"500.003.1001","errorMessage":"Internal Server Error"}`, daraja.ErrUnavailable},
		{200, `{"ResponseCode":"1","ResponseDescription":"failed"}`, daraja.ErrRejected},
		{200, `{"ResponseCode":"00000001","ResponseDescription":"failed"}`, daraja.ErrRejected},
		{200, `not json`, daraja.ErrUnavailable},
	} {
		c := c2bServer(t, func(w http.ResponseWriter, _ string, _ map[string]any) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		})
		if err := c.RegisterC2BURLs(context.Background(), "https://a/v", "https://a/c", daraja.C2BCancelOnTimeout); !errors.Is(err, tc.want) {
			t.Errorf("%d %s: %v, want %v", tc.status, tc.body, err, tc.want)
		}
	}
}

func TestSimulateC2B_PaysIntoThePaybillWithTheAccountNumber(t *testing.T) {
	var got map[string]any
	c := c2bServer(t, func(w http.ResponseWriter, p string, body map[string]any) {
		if p != "/mpesa/c2b/v1/simulate" {
			t.Errorf("path %s", p)
		}
		got = body
		_, _ = w.Write([]byte(`{"OriginatorCoversationID":"o","ResponseCode":"0","ResponseDescription":"Accept the service request successfully."}`))
	})
	if err := c.SimulateC2B(context.Background(), 50, "254708374149", "254712345678"); err != nil {
		t.Fatal(err)
	}
	if got["CommandID"] != "CustomerPayBillOnline" || got["Amount"] != float64(50) || got["Msisdn"] != "254708374149" ||
		got["BillRefNumber"] != "254712345678" || got["ShortCode"] != "600982" {
		t.Fatalf("body %v", got)
	}
	for _, bad := range []struct {
		amount  int64
		msisdn  string
		billRef string
	}{{0, "254708374149", "x"}, {10, "0712", "x"}, {10, "254708374149", ""}, {daraja.MaxSTKAmountKES + 1, "254708374149", "x"}} {
		if err := c.SimulateC2B(context.Background(), bad.amount, bad.msisdn, bad.billRef); !errors.Is(err, daraja.ErrInvalidRequest) {
			t.Errorf("%+v: %v", bad, err)
		}
	}
}

func TestSimulateC2B_IsRefusedAgainstProduction(t *testing.T) {
	cfg := testConfig("https://api.safaricom.co.ke")
	cfg.AllowProduction = true
	c, err := daraja.New(cfg, http.DefaultClient, clock.NewManual(at))
	if err != nil {
		t.Fatal(err)
	}
	if c, err = c.WithC2B(c2bConfig); err != nil {
		t.Fatal(err)
	}
	if err := c.SimulateC2B(context.Background(), 10, "254708374149", "x"); !errors.Is(err, daraja.ErrInvalidRequest) {
		t.Fatalf("err = %v", err)
	}
}

func TestTransactionStatus_AsksAboutTheReceiptAndReturnsTheConversation(t *testing.T) {
	var got map[string]any
	c := c2bServer(t, func(w http.ResponseWriter, p string, body map[string]any) {
		if p != "/mpesa/transactionstatus/v1/query" {
			t.Errorf("path %s", p)
		}
		got = body
		_, _ = w.Write([]byte(`{"OriginatorConversationID":"orig-1","ConversationID":"AG_1","ResponseCode":"0","ResponseDescription":"Accept the service request successfully."}`))
	})
	req, err := c.TransactionStatus(context.Background(), "TJK1AB2CD3")
	if err != nil {
		t.Fatal(err)
	}
	if req != (daraja.StatusRequest{OriginatorConversationID: "orig-1", ConversationID: "AG_1"}) {
		t.Fatalf("req = %+v", req)
	}
	want := map[string]any{
		"Initiator": "testapi", "SecurityCredential": "c2VjcmV0", "CommandID": "TransactionStatusQuery",
		"TransactionID": "TJK1AB2CD3", "PartyA": "600982", "IdentifierType": "4",
		"ResultURL": c2bConfig.ResultURL, "QueueTimeOutURL": c2bConfig.TimeoutURL, "Occasion": "TJK1AB2CD3",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
	if _, err := c.TransactionStatus(context.Background(), "x'; DROP"); !errors.Is(err, daraja.ErrInvalidRequest) {
		t.Fatalf("bad receipt: %v", err)
	}
}

func TestTransactionStatus_ErrorsCarryNoSecrets(t *testing.T) {
	c := c2bServer(t, func(w http.ResponseWriter, _ string, _ map[string]any) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"requestId":"r","errorCode":"400.002.02","errorMessage":"Bad Request - Invalid Initiator c2VjcmV0"}`))
	})
	_, err := c.TransactionStatus(context.Background(), "TJK1AB2CD3")
	if !errors.Is(err, daraja.ErrRejected) {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "c2VjcmV0") || strings.Contains(err.Error(), "testapi") {
		t.Fatalf("error leaks a secret: %v", err)
	}
}

const confirmation = `{"TransactionType":"Pay Bill","TransID":"TJK1AB2CD3","TransTime":"20261024093112","TransAmount":"150.00",
"BusinessShortCode":"600982","BillRefNumber":" 0712345678 ","InvoiceNumber":"","OrgAccountBalance":"","ThirdPartyTransID":"",
"MSISDN":"254708374149","FirstName":"John","MiddleName":"","LastName":"Doe"}`

func TestParseC2BNotification_ReadsAConfirmation(t *testing.T) {
	p, err := daraja.ParseC2BNotification([]byte(confirmation))
	if err != nil {
		t.Fatal(err)
	}
	want := daraja.C2BPayment{TransID: "TJK1AB2CD3", TransType: "Pay Bill", TransTime: "20261024093112", AmountKES: 150,
		ShortCode: "600982", BillRef: "0712345678", MSISDN: "254708374149", FirstName: "John"}
	if p != want {
		t.Fatalf("p = %+v\nwant %+v", p, want)
	}
}

func TestParseC2BNotification_IgnoresMaskedPayersAndRefusesBadBodies(t *testing.T) {
	masked := strings.Replace(confirmation, `"254708374149"`, `"2547 ***** 149"`, 1)
	p, err := daraja.ParseC2BNotification([]byte(masked))
	if err != nil || p.MSISDN != "" {
		t.Fatalf("masked: %+v %v", p, err)
	}
	hashed := strings.Replace(confirmation, `"254708374149"`, `"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"`, 1)
	if p, err := daraja.ParseC2BNotification([]byte(hashed)); err != nil || p.MSISDN != "" {
		t.Fatalf("hashed: %+v %v", p, err)
	}
	for name, body := range map[string]string{
		"not json":     `{`,
		"fractional":   strings.Replace(confirmation, `"150.00"`, `"150.50"`, 1),
		"zero":         strings.Replace(confirmation, `"150.00"`, `"0"`, 1),
		"too big":      strings.Replace(confirmation, `"150.00"`, `"250001"`, 1),
		"no receipt":   strings.Replace(confirmation, `"TJK1AB2CD3"`, `""`, 1),
		"bad receipt":  strings.Replace(confirmation, `"TJK1AB2CD3"`, `"tjk1ab2cd3';"`, 1),
		"no shortcode": strings.Replace(confirmation, `"600982"`, `""`, 1),
	} {
		if _, err := daraja.ParseC2BNotification([]byte(body)); !errors.Is(err, daraja.ErrBadCallback) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := daraja.ParseC2BNotification([]byte(strings.Replace(confirmation, `"150.00"`, `"1.5"`, 1))); !errors.Is(err, daraja.ErrBadAmount) {
		t.Fatalf("bad amount is not marked: %v", err)
	}
	// a numeric amount works too
	if p, err := daraja.ParseC2BNotification([]byte(strings.Replace(confirmation, `"150.00"`, `150`, 1))); err != nil || p.AmountKES != 150 {
		t.Fatalf("numeric amount: %+v %v", p, err)
	}
}

const statusResult = `{"Result":{"ResultType":0,"ResultCode":0,"ResultDesc":"The service request is processed successfully.",
"OriginatorConversationID":"orig-1","ConversationID":"AG_1","TransactionID":"TJK0000000",
"ResultParameters":{"ResultParameter":[{"Key":"DebitPartyName","Value":"254708374149 - John Doe"},
{"Key":"CreditPartyName","Value":"600982 - Safaricom Test"},{"Key":"OriginatorConversationID","Value":"x"},
{"Key":"InitiatedTime","Value":20261024093112},{"Key":"DebitAccountType","Value":"MMF Account For Customer"},
{"Key":"DebitPartyCharges"},{"Key":"TransactionReason"},{"Key":"ReasonType","Value":"Pay Bill Online"},
{"Key":"TransactionStatus","Value":"Completed"},{"Key":"FinalisedTime","Value":20261024093112},
{"Key":"Amount","Value":150},{"Key":"ConversationID","Value":"AG_2"},{"Key":"ReceiptNo","Value":"TJK1AB2CD3"}]},
"ReferenceData":{"ReferenceItem":{"Key":"Occasion","Value":"TJK1AB2CD3"}}}}`

func TestParseStatusResult_ReadsACompletedTransaction(t *testing.T) {
	r, err := daraja.ParseStatusResult([]byte(statusResult))
	if err != nil {
		t.Fatal(err)
	}
	if !r.Completed() || r.Receipt != "TJK1AB2CD3" || r.AmountKES != 150 || r.OriginatorConversationID != "orig-1" ||
		r.ConversationID != "AG_1" || r.Occasion != "TJK1AB2CD3" || !r.CreditedTo("600982") || r.CreditedTo("174379") || r.Payer() != "254708374149" {
		t.Fatalf("r = %+v", r)
	}
}

func TestParseStatusResult_FailuresAndOtherStates(t *testing.T) {
	r, err := daraja.ParseStatusResult([]byte(`{"Result":{"ResultType":0,"ResultCode":2001,"ResultDesc":"The initiator information is invalid.",
"OriginatorConversationID":"orig-2","ConversationID":"AG_3","TransactionID":"X"}}`))
	if err != nil || r.Completed() || r.ResultCode != 2001 || r.Receipt != "" {
		t.Fatalf("failure: %+v %v", r, err)
	}
	reversed := strings.Replace(statusResult, `"Completed"`, `"Reversed"`, 1)
	if r, err := daraja.ParseStatusResult([]byte(reversed)); err != nil || r.Completed() || r.Status != "Reversed" {
		t.Fatalf("reversed: %+v %v", r, err)
	}
	masked := strings.Replace(statusResult, `"254708374149 - John Doe"`, `"2547****149 - John Doe"`, 1)
	if r, err := daraja.ParseStatusResult([]byte(masked)); err != nil || r.Payer() != "" {
		t.Fatalf("masked payer: %+v %v", r, err)
	}
	single := `{"Result":{"ResultCode":0,"OriginatorConversationID":"o","ResultParameters":{"ResultParameter":{"Key":"TransactionStatus","Value":"Completed"}}}}`
	if _, err := daraja.ParseStatusResult([]byte(single)); !errors.Is(err, daraja.ErrBadCallback) {
		t.Fatalf("completed without receipt: %v", err)
	}
	for name, body := range map[string]string{
		"not json":       `[`,
		"no code":        `{"Result":{"OriginatorConversationID":"o"}}`,
		"no originator":  `{"Result":{"ResultCode":0}}`,
		"fractional":     strings.Replace(statusResult, `{"Key":"Amount","Value":150}`, `{"Key":"Amount","Value":150.5}`, 1),
		"without amount": strings.Replace(statusResult, `{"Key":"Amount","Value":150},`, ``, 1),
	} {
		if _, err := daraja.ParseStatusResult([]byte(body)); !errors.Is(err, daraja.ErrBadCallback) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
