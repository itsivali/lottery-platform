package darajamock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// C2BFault shapes the next paybill payment (and only the next one) and the Transaction Status answers about it. The
// zero value is a clean payment that Daraja confirms.
type C2BFault struct {
	DropConfirmation      bool   `json:"drop_confirmation"`      // take the money but never call the confirmation URL
	DuplicateConfirmation bool   `json:"duplicate_confirmation"` // call the confirmation URL twice
	MaskPhone             bool   `json:"mask_phone"`             // report the payer as a masked number
	StatusHTTP            int    `json:"status_http"`            // answer status requests with this HTTP status
	StatusResultCode      int    `json:"status_result_code"`     // non-zero: the status result is a failure
	StatusAmountKES       int64  `json:"status_amount_kes"`      // the amount the status result reports, when it differs
	StatusState           string `json:"status_state"`           // the TransactionStatus reported instead of "Completed"
	StatusPayer           string `json:"status_payer"`           // the debit party number reported instead of the payer
	StatusTimeout         bool   `json:"status_timeout"`         // post to the timeout URL instead of the result URL
	StatusDrop            bool   `json:"status_drop"`            // accept status requests but never answer
}

// PayBill is a paybill payment the mock took.
type PayBill struct {
	Receipt   string
	AmountKES int64
	MSISDN    string
	BillRef   string
	Cancelled bool // the validation URL refused it, so no money moved
}

type c2bTxn struct {
	PayBill
	fault C2BFault
}

// c2bForbidden are words Daraja refuses in C2B URLs.
var c2bForbidden = []string{"mpesa", "m-pesa", "safaricom", "exec", "cmd", "sql", "query"}

func (s *Server) bearerOnly(h func(http.ResponseWriter, *http.Request, map[string]any)) http.HandlerFunc {
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
		h(w, r, body)
	}
}

func (s *Server) registerURLs(w http.ResponseWriter, _ *http.Request, body map[string]any) {
	code, _ := body["ShortCode"].(string)
	rt, _ := body["ResponseType"].(string)
	v, _ := body["ValidationURL"].(string)
	c, _ := body["ConfirmationURL"].(string)
	switch {
	case s.opts.C2BShortCode == "" || code != s.opts.C2BShortCode:
		writeErr(w, http.StatusBadRequest, "400.003.01", "Invalid ShortCode")
		return
	case rt != "Cancelled" && rt != "Completed":
		writeErr(w, http.StatusBadRequest, "400.003.02", "Bad Request - Invalid ResponseType")
		return
	case v == "" || c == "" || forbiddenURL(v) || forbiddenURL(c):
		writeErr(w, http.StatusBadRequest, "400.003.02", "Bad Request - Invalid URL")
		return
	}
	s.mu.Lock()
	s.validationURL, s.confirmationURL, s.responseType = v, c, rt
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]string{"OriginatorCoversationID": "mock-reg", "ResponseCode": "0", "ResponseDescription": "success"})
}

func forbiddenURL(u string) bool {
	l := strings.ToLower(u)
	for _, f := range c2bForbidden {
		if strings.Contains(l, f) {
			return true
		}
	}
	return false
}

// SetNextC2BFault makes the next paybill payment behave as f.
func (s *Server) SetNextC2BFault(f C2BFault) {
	s.mu.Lock()
	s.nextC2B = f
	s.mu.Unlock()
}

// RegisteredC2BURLs returns the registered validation and confirmation URLs.
func (s *Server) RegisteredC2BURLs() (validation, confirmation string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.validationURL, s.confirmationURL
}

// PayBills returns the paybill payments in order.
func (s *Server) PayBills() []PayBill {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]PayBill, 0, len(s.c2bOrder))
	for _, r := range s.c2bOrder {
		out = append(out, s.c2bTxns[r].PayBill)
	}
	return out
}

func (s *Server) simulate(w http.ResponseWriter, _ *http.Request, body map[string]any) {
	code := fmt.Sprint(body["ShortCode"])
	amount, _ := body["Amount"].(float64)
	msisdn, _ := body["Msisdn"].(string)
	ref, _ := body["BillRefNumber"].(string)
	if s.opts.C2BShortCode == "" || code != s.opts.C2BShortCode || amount <= 0 || msisdn == "" {
		writeErr(w, http.StatusBadRequest, "400.002.02", "Bad Request - Invalid request")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"OriginatorCoversationID": "mock-sim", "ResponseCode": "0", "ResponseDescription": "Accept the service request successfully."})
	s.async(func(ctx context.Context) { _, _ = s.Pay(ctx, int64(amount), msisdn, ref) })
}

// Pay takes a paybill payment as a customer would make it from the M-Pesa menu: the validation URL is asked first
// (when ExternalValidation is on) and may refuse it; otherwise the money moves and the confirmation URL is called as
// the payment's fault says. It returns the receipt. An error means no URLs are registered, or a URL did not answer 2xx.
func (s *Server) Pay(ctx context.Context, amount int64, msisdn, billRef string) (string, error) {
	s.mu.Lock()
	vURL, cURL := s.validationURL, s.confirmationURL
	if cURL == "" {
		s.mu.Unlock()
		return "", errors.New("darajamock: no C2B URLs registered")
	}
	f := s.nextC2B
	s.nextC2B = C2BFault{}
	s.c2bSeq++
	t := &c2bTxn{fault: f, PayBill: PayBill{Receipt: "MCC" + strconv.Itoa(10_000_000 + s.c2bSeq)[1:], AmountKES: amount, MSISDN: msisdn, BillRef: billRef}}
	s.c2bTxns[t.Receipt] = t
	s.c2bOrder = append(s.c2bOrder, t.Receipt)
	s.mu.Unlock()

	payer := msisdn
	if f.MaskPhone && len(msisdn) == 12 {
		payer = msisdn[:4] + "****" + msisdn[len(msisdn)-3:]
	}
	body, err := json.Marshal(map[string]string{
		"TransactionType": "Pay Bill", "TransID": t.Receipt, "TransTime": "20261024093112",
		"TransAmount": strconv.FormatInt(amount, 10) + ".00", "BusinessShortCode": s.opts.C2BShortCode,
		"BillRefNumber": billRef, "InvoiceNumber": "", "OrgAccountBalance": "", "ThirdPartyTransID": "",
		"MSISDN": payer, "FirstName": "John", "MiddleName": "", "LastName": "Doe",
	})
	if err != nil {
		return "", err
	}
	if s.opts.ExternalValidation && vURL != "" {
		ok, err := s.validate(ctx, vURL, body)
		if err != nil || !ok {
			s.mu.Lock()
			t.Cancelled = true
			s.mu.Unlock()
			return t.Receipt, err
		}
	}
	if f.DropConfirmation {
		return t.Receipt, nil
	}
	n := 1
	if f.DuplicateConfirmation {
		n = 2
	}
	for range n {
		if err := s.post(ctx, cURL, body); err != nil {
			return t.Receipt, err
		}
	}
	return t.Receipt, nil
}

func (s *Server) validate(ctx context.Context, url string, body []byte) (bool, error) {
	var out struct {
		ResultCode any `json:"ResultCode"`
	}
	if err := s.postFor(ctx, url, body, &out); err != nil {
		return false, err
	}
	return fmt.Sprint(out.ResultCode) == "0", nil
}

func (s *Server) transactionStatus(w http.ResponseWriter, _ *http.Request, body map[string]any) {
	if s.opts.Initiator == "" || body["Initiator"] != s.opts.Initiator || body["SecurityCredential"] != s.opts.SecurityCredential {
		writeErr(w, http.StatusBadRequest, "400.002.02", "Bad Request - Invalid Initiator")
		return
	}
	if body["PartyA"] != s.opts.C2BShortCode || body["CommandID"] != "TransactionStatusQuery" {
		writeErr(w, http.StatusBadRequest, "400.002.02", "Bad Request - Invalid PartyA")
		return
	}
	receipt, _ := body["TransactionID"].(string)
	resultURL, _ := body["ResultURL"].(string)
	timeoutURL, _ := body["QueueTimeOutURL"].(string)
	s.mu.Lock()
	t, known := s.c2bTxns[receipt]
	var f C2BFault
	var pb PayBill
	if known {
		f, pb = t.fault, t.PayBill
	}
	s.statusSeq++
	orig, conv := "mock-ts-"+strconv.Itoa(s.statusSeq), "AG_MOCK_"+strconv.Itoa(s.statusSeq)
	s.statusRequests = append(s.statusRequests, receipt)
	s.mu.Unlock()

	if f.StatusHTTP != 0 {
		writeErr(w, f.StatusHTTP, "500.003.1001", "Service unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"OriginatorConversationID": orig, "ConversationID": conv,
		"ResponseCode": "0", "ResponseDescription": "Accept the service request successfully."})
	if f.StatusDrop {
		return
	}
	occasion, _ := body["Occasion"].(string)
	result := map[string]any{"ResultType": 0, "OriginatorConversationID": orig, "ConversationID": conv, "TransactionID": "MCKTS00000",
		"ReferenceData": map[string]any{"ReferenceItem": map[string]any{"Key": "Occasion", "Value": occasion}}}
	url := resultURL
	switch {
	case f.StatusTimeout:
		url = timeoutURL
		result["ResultType"], result["ResultCode"], result["ResultDesc"] = 1, 1037, "Queue timeout"
	case !known || pb.Cancelled:
		result["ResultCode"], result["ResultDesc"] = 1, "The transaction receipt number does not exist."
	case f.StatusResultCode != 0:
		result["ResultCode"], result["ResultDesc"] = f.StatusResultCode, resultDesc(f.StatusResultCode)
	default:
		amount, state, payer := pb.AmountKES, "Completed", pb.MSISDN
		if f.StatusAmountKES != 0 {
			amount = f.StatusAmountKES
		}
		if f.StatusState != "" {
			state = f.StatusState
		}
		if f.StatusPayer != "" {
			payer = f.StatusPayer
		}
		if f.MaskPhone && len(payer) == 12 {
			payer = payer[:4] + "****" + payer[len(payer)-3:]
		}
		result["ResultCode"], result["ResultDesc"] = 0, "The service request is processed successfully."
		result["ResultParameters"] = map[string]any{"ResultParameter": []map[string]any{
			{"Key": "DebitPartyName", "Value": payer + " - John Doe"},
			{"Key": "CreditPartyName", "Value": s.opts.C2BShortCode + " - Mock Paybill"},
			{"Key": "TransactionStatus", "Value": state},
			{"Key": "ReasonType", "Value": "Pay Bill Online"},
			{"Key": "Amount", "Value": amount},
			{"Key": "ReceiptNo", "Value": receipt},
		}}
	}
	out, err := json.Marshal(map[string]any{"Result": result})
	if err != nil {
		return
	}
	s.async(func(ctx context.Context) { _ = s.post(ctx, url, out) })
}

// StatusRequests returns the receipts Transaction Status was asked about, in order.
func (s *Server) StatusRequests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.statusRequests...)
}

// async runs f in the background, as Daraja answers asynchronous requests. Flush waits for it.
func (s *Server) async(f func(context.Context)) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		f(context.Background())
	}()
}

// Flush waits until every asynchronous answer (simulated payments, status results) has been delivered.
func (s *Server) Flush() { s.wg.Wait() }
