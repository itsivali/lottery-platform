// Package darajamock is an in-memory Daraja (M-Pesa) server for tests and local development. It speaks the same OAuth,
// STK Push and STK query contract as Safaricom, posts STK callbacks, and can inject the faults the payments module must
// survive: cancelled or failed prompts, lost and duplicated callbacks, a paid amount that differs from the request,
// masked phone numbers, and push or query outages.
//
// A push stays pending until it is completed, either explicitly (Complete, or POST /__mock/complete/{checkoutID}) so
// tests are deterministic, or automatically after a delay (AutoComplete) for the compose stack.
package darajamock

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Fault shapes the next push (and only the next one). The zero value is a clean, paid transaction.
type Fault struct {
	PushStatus        int   `json:"push_status"`        // answer the push with this HTTP status instead of accepting it
	QueryStatus       int   `json:"query_status"`       // answer status queries for this push with this HTTP status
	ResultCode        int   `json:"result_code"`        // non-zero: the customer cancelled (1032), timed out (1037), ...
	PaidAmountKES     int64 `json:"paid_amount_kes"`    // the amount the callback reports, when it differs from the request
	DropCallback      bool  `json:"drop_callback"`      // complete the payment but never call back
	DuplicateCallback bool  `json:"duplicate_callback"` // call back twice with the same body
	MaskPhone         bool  `json:"mask_phone"`
	PendingAs4999     bool  `json:"pending_as_4999"` // answer queries before completion as the sandbox does: 200, ResultCode 4999         // report the payer as a masked number
}

// Push is a push the mock accepted.
type Push struct {
	CheckoutRequestID string
	MerchantRequestID string
	MSISDN            string
	AmountKES         int64
	AccountRef        string
	CallbackURL       string
}

type txn struct {
	Push
	fault     Fault
	completed bool
}

// Options configures the mock. Credentials are what clients must present.
type Options struct {
	ConsumerKey, ConsumerSecret, ShortCode, Passkey string
	// HTTPClient posts the callbacks (http.DefaultClient when nil).
	HTTPClient *http.Client
}

// Server is the mock. It implements http.Handler.
type Server struct {
	opts Options
	mux  *http.ServeMux

	mu        sync.Mutex
	seq       int
	next      Fault
	txns      map[string]*txn
	order     []string
	autoDelay time.Duration
	autoCtx   context.Context
	auto      bool
}

// New returns a mock server.
func New(o Options) *Server {
	if o.HTTPClient == nil {
		o.HTTPClient = http.DefaultClient
	}
	s := &Server{opts: o, txns: map[string]*txn{}, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("daraja-mock ok")) })
	s.mux.HandleFunc("GET /oauth/v1/generate", s.token)
	s.mux.HandleFunc("POST /mpesa/stkpush/v1/processrequest", s.authed(s.push))
	s.mux.HandleFunc("POST /mpesa/stkpushquery/v1/query", s.authed(s.query))
	s.mux.HandleFunc("POST /__mock/fault", s.setFault)
	s.mux.HandleFunc("POST /__mock/complete/{id}", s.complete)
	return s
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// SetNextFault makes the next push behave as f.
func (s *Server) SetNextFault(f Fault) {
	s.mu.Lock()
	s.next = f
	s.mu.Unlock()
}

// AutoComplete completes every later push after delay, in the background, until ctx ends.
func (s *Server) AutoComplete(ctx context.Context, delay time.Duration) {
	s.mu.Lock()
	s.auto, s.autoDelay, s.autoCtx = true, delay, ctx
	s.mu.Unlock()
}

// Pushes returns the accepted pushes in order.
func (s *Server) Pushes() []Push {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Push, 0, len(s.order))
	for _, id := range s.order {
		out = append(out, s.txns[id].Push)
	}
	return out
}

const bearer = "Bearer mock-access-token" //nolint:gosec // the mock's fixed, public test token

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	u, p, ok := r.BasicAuth()
	if !ok || u != s.opts.ConsumerKey || p != s.opts.ConsumerSecret || r.URL.Query().Get("grant_type") != "client_credentials" {
		writeErr(w, http.StatusBadRequest, "400.008.01", "Invalid Authentication passed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"access_token": "mock-access-token", "expires_in": "3599"})
}

func (s *Server) authed(h func(http.ResponseWriter, *http.Request, map[string]any)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != bearer {
			writeErr(w, http.StatusUnauthorized, "404.001.03", "Invalid Access Token")
			return
		}
		var body map[string]any
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "400.002.02", "Bad Request - Invalid JSON")
			return
		}
		ts, _ := body["Timestamp"].(string)
		want := base64.StdEncoding.EncodeToString([]byte(s.opts.ShortCode + s.opts.Passkey + ts))
		if body["BusinessShortCode"] != s.opts.ShortCode || body["Password"] != want {
			writeErr(w, http.StatusBadRequest, "400.002.02", "Bad Request - Invalid Password")
			return
		}
		h(w, r, body)
	}
}

func (s *Server) push(w http.ResponseWriter, _ *http.Request, body map[string]any) {
	s.mu.Lock()
	f := s.next
	s.next = Fault{}
	if f.PushStatus != 0 {
		s.mu.Unlock()
		writeErr(w, f.PushStatus, "500.001.1001", "Unable to lock subscriber, a transaction is already in process for the current subscriber")
		return
	}
	amount, _ := body["Amount"].(float64)
	msisdn, _ := body["PhoneNumber"].(string)
	ref, _ := body["AccountReference"].(string)
	cb, _ := body["CallBackURL"].(string)
	if amount <= 0 || msisdn == "" || cb == "" {
		s.mu.Unlock()
		writeErr(w, http.StatusBadRequest, "400.002.02", "Bad Request - Invalid Amount or PhoneNumber")
		return
	}
	s.seq++
	t := &txn{fault: f, Push: Push{
		CheckoutRequestID: "ws_CO_MOCK_" + strconv.Itoa(s.seq), MerchantRequestID: "mock-" + strconv.Itoa(s.seq),
		MSISDN: msisdn, AmountKES: int64(amount), AccountRef: ref, CallbackURL: cb,
	}}
	s.txns[t.CheckoutRequestID] = t
	s.order = append(s.order, t.CheckoutRequestID)
	auto, delay, ctx := s.auto, s.autoDelay, s.autoCtx
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]string{
		"MerchantRequestID": t.MerchantRequestID, "CheckoutRequestID": t.CheckoutRequestID, "ResponseCode": "0",
		"ResponseDescription": "Success. Request accepted for processing", "CustomerMessage": "Success. Request accepted for processing",
	})
	if auto {
		id := t.CheckoutRequestID
		time.AfterFunc(delay, func() {
			if ctx.Err() == nil {
				_ = s.Complete(ctx, id)
			}
		})
	}
}

func (s *Server) query(w http.ResponseWriter, _ *http.Request, body map[string]any) {
	id, _ := body["CheckoutRequestID"].(string)
	s.mu.Lock()
	t, ok := s.txns[id]
	var f Fault
	var done bool
	if ok {
		f, done = t.fault, t.completed
	}
	s.mu.Unlock()
	switch {
	case !ok:
		writeErr(w, http.StatusBadRequest, "400.002.02", "Bad Request - Invalid CheckoutRequestID")
	case f.QueryStatus != 0:
		writeErr(w, f.QueryStatus, "503.001.01", "Service unavailable")
	case !done && f.PendingAs4999:
		writeJSON(w, http.StatusOK, map[string]string{
			"ResponseCode": "0", "ResponseDescription": "The service request has been accepted successsfully",
			"MerchantRequestID": t.MerchantRequestID, "CheckoutRequestID": id,
			"ResultCode": "4999", "ResultDesc": "The transaction is still under processing",
		})
	case !done:
		writeErr(w, http.StatusInternalServerError, "500.001.1001", "The transaction is being processed")
	default:
		writeJSON(w, http.StatusOK, map[string]string{
			"ResponseCode": "0", "ResponseDescription": "The service request has been accepted successsfully",
			"MerchantRequestID": t.MerchantRequestID, "CheckoutRequestID": id,
			"ResultCode": strconv.Itoa(f.ResultCode), "ResultDesc": resultDesc(f.ResultCode),
		})
	}
}

// Complete settles a pending push as its fault says and posts the callback (unless the fault drops it). An error
// means the push is unknown or the receiver did not answer 2xx.
func (s *Server) Complete(ctx context.Context, checkoutID string) error {
	s.mu.Lock()
	t, ok := s.txns[checkoutID]
	if !ok {
		s.mu.Unlock()
		return errors.New("darajamock: unknown checkout id")
	}
	t.completed = true
	f, p, receipt := t.fault, t.Push, fmt.Sprint("MCK", 100000+s.seqFor(checkoutID))
	s.mu.Unlock()

	if f.DropCallback {
		return nil
	}
	body, err := json.Marshal(callbackBody(p, f, receipt))
	if err != nil {
		return err
	}
	n := 1
	if f.DuplicateCallback {
		n = 2
	}
	for range n {
		if err := s.post(ctx, p.CallbackURL, body); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) seqFor(checkoutID string) int {
	for i, id := range s.order {
		if id == checkoutID {
			return i + 1
		}
	}
	return 0
}

func (s *Server) post(ctx context.Context, url string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.opts.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("darajamock: callback answered %d", resp.StatusCode)
	}
	return nil
}

func callbackBody(p Push, f Fault, receipt string) map[string]any {
	stk := map[string]any{
		"MerchantRequestID": p.MerchantRequestID, "CheckoutRequestID": p.CheckoutRequestID,
		"ResultCode": f.ResultCode, "ResultDesc": resultDesc(f.ResultCode),
	}
	if f.ResultCode == 0 {
		amount := p.AmountKES
		if f.PaidAmountKES != 0 {
			amount = f.PaidAmountKES
		}
		var phone any
		if phone, _ = strconv.ParseInt(p.MSISDN, 10, 64); f.MaskPhone {
			phone = p.MSISDN[:4] + " ***** " + p.MSISDN[len(p.MSISDN)-3:]
		}
		stk["CallbackMetadata"] = map[string]any{"Item": []map[string]any{
			{"Name": "Amount", "Value": amount},
			{"Name": "MpesaReceiptNumber", "Value": receipt},
			{"Name": "Balance"},
			{"Name": "TransactionDate", "Value": 20261022093112},
			{"Name": "PhoneNumber", "Value": phone},
		}}
	}
	return map[string]any{"Body": map[string]any{"stkCallback": stk}}
}

func resultDesc(code int) string {
	switch code {
	case 0:
		return "The service request is processed successfully."
	case 1:
		return "The balance is insufficient for the transaction."
	case 1032:
		return "Request cancelled by user"
	case 1037:
		return "DS timeout user cannot be reached"
	case 2001:
		return "The initiator information is invalid."
	default:
		return "Transaction failed"
	}
}

func (s *Server) setFault(w http.ResponseWriter, r *http.Request) {
	var f Fault
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&f); err != nil {
		http.Error(w, "bad fault", http.StatusBadRequest)
		return
	}
	s.SetNextFault(f)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) complete(w http.ResponseWriter, r *http.Request) {
	if err := s.Complete(r.Context(), r.PathValue("id")); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"requestId": "mock", "errorCode": code, "errorMessage": msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
