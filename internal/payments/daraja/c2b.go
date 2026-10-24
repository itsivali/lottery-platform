package daraja

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// C2BConfig is the paybill (customer-to-business) side of the app: the shortcode customers pay into and the
// initiator that may ask Daraja for a transaction's status. URLs are where Daraja posts Transaction Status results.
type C2BConfig struct {
	ShortCode string
	// Initiator is the API operator created on the M-Pesa org portal (the sandbox's Test Credentials page).
	Initiator string
	// SecurityCredential is the initiator's password encrypted with Safaricom's certificate and base64-encoded, as
	// the Daraja portal generates it. It is a secret.
	SecurityCredential string
	ResultURL          string
	TimeoutURL         string
}

func (c C2BConfig) validate(production bool) error {
	for name, v := range map[string]string{
		"c2b shortcode": c.ShortCode, "initiator": c.Initiator, "security credential": c.SecurityCredential,
		"status result url": c.ResultURL, "status timeout url": c.TimeoutURL,
	} {
		if v == "" {
			return fmt.Errorf("daraja: %s is required", name)
		}
	}
	for _, raw := range []string{c.ResultURL, c.TimeoutURL} {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return errors.New("daraja: status urls must be absolute")
		}
		if production && u.Scheme != "https" {
			return errors.New("daraja: Safaricom calls back over https only")
		}
	}
	return nil
}

// WithC2B enables the paybill calls. Without it they return ErrInvalidRequest.
func (c *Client) WithC2B(cfg C2BConfig) (*Client, error) {
	base, _ := url.Parse(c.cfg.BaseURL)
	if err := cfg.validate(strings.HasSuffix(base.Hostname(), "safaricom.co.ke")); err != nil {
		return nil, err
	}
	c.c2b = &cfg
	return c, nil
}

// C2B response types: what M-Pesa does with a payment when our validation URL cannot be reached.
const (
	C2BCancelOnTimeout   = "Cancelled"
	C2BCompleteOnTimeout = "Completed"
)

// RegisterC2BURLs tells Daraja where to send validation requests and payment confirmations for the paybill. In
// production a shortcode's URLs can be registered once (Safaricom must clear them before a change); the sandbox accepts
// repeated registrations. Daraja refuses URLs containing words such as "mpesa", "safaricom", "exec", "cmd" or "sql".
func (c *Client) RegisterC2BURLs(ctx context.Context, validationURL, confirmationURL, responseType string) error {
	if c.c2b == nil || validationURL == "" || confirmationURL == "" ||
		(responseType != C2BCancelOnTimeout && responseType != C2BCompleteOnTimeout) {
		return ErrInvalidRequest
	}
	const path = "/mpesa/c2b/v1/registerurl"
	status, raw, err := c.call(ctx, path, map[string]any{
		"ShortCode": c.c2b.ShortCode, "ResponseType": responseType,
		"ConfirmationURL": confirmationURL, "ValidationURL": validationURL,
	})
	if err != nil {
		return err
	}
	return c.accepted(path, status, raw)
}

// SimulateC2B makes the sandbox pay amount into the paybill from msisdn with the given account number, as a customer
// would from the M-Pesa menu. Daraja then calls the registered URLs. It is refused against production.
func (c *Client) SimulateC2B(ctx context.Context, amount int64, msisdn, billRef string) error {
	if c.c2b == nil || amount <= 0 || amount > MaxSTKAmountKES || !msisdnRE.MatchString(msisdn) || billRef == "" {
		return ErrInvalidRequest
	}
	if c.production() {
		return fmt.Errorf("%w: simulation exists only in the sandbox", ErrInvalidRequest)
	}
	const path = "/mpesa/c2b/v1/simulate"
	status, raw, err := c.call(ctx, path, map[string]any{
		"ShortCode": c.c2b.ShortCode, "CommandID": "CustomerPayBillOnline", "Amount": amount,
		"Msisdn": msisdn, "BillRefNumber": billRef,
	})
	if err != nil {
		return err
	}
	return c.accepted(path, status, raw)
}

func (c *Client) production() bool {
	u, _ := url.Parse(c.cfg.BaseURL)
	return u.Hostname() == "api.safaricom.co.ke"
}

// accepted checks the synchronous answer of an asynchronous Daraja request.
func (c *Client) accepted(path string, status int, raw []byte) error {
	if status != http.StatusOK {
		return c.classify(path, status, raw)
	}
	var out struct {
		ResponseCode string `json:"ResponseCode"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.ResponseCode == "" {
		return fmt.Errorf("%w: unreadable %s answer", ErrUnavailable, path)
	}
	// "0" means accepted; the sandbox's URL registration answers "00000000" (seen live 2026-10-24)
	if strings.Trim(out.ResponseCode, "0") != "" {
		return fmt.Errorf("%w: %s response code %s", ErrRejected, path, out.ResponseCode)
	}
	return nil
}

// StatusRequest identifies an accepted Transaction Status request; the answer arrives later at the result URL.
type StatusRequest struct {
	OriginatorConversationID string
	ConversationID           string
}

// TransactionStatus asks Daraja whether the M-Pesa transaction with this receipt completed, and for how much. Daraja
// answers asynchronously at the configured result URL (or the timeout URL when its queue gives up). Asking again is
// harmless: the query moves no money.
func (c *Client) TransactionStatus(ctx context.Context, receipt string) (StatusRequest, error) {
	if c.c2b == nil || !receiptRE.MatchString(receipt) {
		return StatusRequest{}, ErrInvalidRequest
	}
	const path = "/mpesa/transactionstatus/v1/query"
	status, raw, err := c.call(ctx, path, map[string]any{
		"Initiator": c.c2b.Initiator, "SecurityCredential": c.c2b.SecurityCredential,
		"CommandID": "TransactionStatusQuery", "TransactionID": receipt, "OriginalConversationID": "",
		"PartyA": c.c2b.ShortCode, "IdentifierType": "4", "ResultURL": c.c2b.ResultURL,
		"QueueTimeOutURL": c.c2b.TimeoutURL, "Remarks": "Paybill check",
		// Daraja echoes the occasion in the result, which ties a result to its receipt even when it names no receipt
		"Occasion": receipt,
	})
	if err != nil {
		return StatusRequest{}, err
	}
	if err := c.accepted(path, status, raw); err != nil {
		return StatusRequest{}, err
	}
	var out struct {
		OriginatorConversationID string `json:"OriginatorConversationID"`
		ConversationID           string `json:"ConversationID"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.OriginatorConversationID == "" {
		return StatusRequest{}, fmt.Errorf("%w: status answer without a conversation id", ErrUnavailable)
	}
	return StatusRequest{OriginatorConversationID: out.OriginatorConversationID, ConversationID: out.ConversationID}, nil
}

// M-Pesa receipts are ten upper-case letters and digits; the check keeps anything else out of request bodies.
var receiptRE = regexp.MustCompile(`^[A-Z0-9]{10}$`)

// C2BPayment is a paybill payment as Daraja reports it to the validation and confirmation URLs. Like an STK callback it
// is a claim: the Transaction Status API confirms it before money moves.
type C2BPayment struct {
	TransID   string // the M-Pesa receipt
	TransType string
	TransTime string
	AmountKES int64
	ShortCode string
	BillRef   string // the account number the customer typed
	// MSISDN is the payer; empty when absent, masked or hashed (Daraja hides it on newer API versions).
	MSISDN    string
	FirstName string
}

// ParseC2BNotification reads a validation or confirmation body. The amount must be a positive whole number of shillings.
func ParseC2BNotification(body []byte) (C2BPayment, error) {
	var in struct {
		TransactionType   string          `json:"TransactionType"`
		TransID           string          `json:"TransID"`
		TransTime         string          `json:"TransTime"`
		TransAmount       json.RawMessage `json:"TransAmount"`
		BusinessShortCode string          `json:"BusinessShortCode"`
		BillRefNumber     string          `json:"BillRefNumber"`
		MSISDN            json.RawMessage `json:"MSISDN"`
		FirstName         string          `json:"FirstName"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return C2BPayment{}, fmt.Errorf("%w: not json", ErrBadCallback)
	}
	if !receiptRE.MatchString(in.TransID) || in.BusinessShortCode == "" {
		return C2BPayment{}, fmt.Errorf("%w: missing receipt or shortcode", ErrBadCallback)
	}
	amount, ok := wholeKES(in.TransAmount)
	if !ok {
		return C2BPayment{}, fmt.Errorf("%w: %w", ErrBadCallback, ErrBadAmount)
	}
	p := C2BPayment{
		TransID: in.TransID, TransType: in.TransactionType, TransTime: in.TransTime, AmountKES: amount,
		ShortCode: in.BusinessShortCode, BillRef: strings.TrimSpace(in.BillRefNumber), FirstName: in.FirstName,
	}
	if n := strings.Trim(string(in.MSISDN), `"`); msisdnRE.MatchString(n) {
		p.MSISDN = n
	}
	return p, nil
}

// wholeKES reads an amount Daraja sends as a number or a numeric string ("10", "10.00", 10.0).
func wholeKES(raw json.RawMessage) (int64, bool) {
	s := strings.Trim(string(raw), `"`)
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f != math.Trunc(f) || f <= 0 || f > MaxSTKAmountKES {
		return 0, false
	}
	return int64(f), true
}

// StatusResult is Daraja's answer to a Transaction Status request.
type StatusResult struct {
	ResultCode               int32
	ResultDesc               string
	OriginatorConversationID string
	ConversationID           string
	// Set when the transaction was found.
	Receipt     string
	AmountKES   int64
	Occasion    string // what the request sent as its occasion: the receipt asked about
	Status      string // "Completed", or another M-Pesa state (a reversal, a decline...)
	CreditParty string // "<shortcode> - <name>": who received the money
	DebitParty  string // "<msisdn> - <name>": who paid; the number may be masked
}

// Completed reports whether Daraja confirms a completed transaction.
func (r StatusResult) Completed() bool { return r.ResultCode == 0 && r.Status == "Completed" }

// CreditedTo reports whether the money went to shortcode. A result without a credit party says nothing either way.
func (r StatusResult) CreditedTo(shortcode string) bool {
	if r.CreditParty == "" {
		return true
	}
	code, _, _ := strings.Cut(r.CreditParty, " ")
	return code == shortcode
}

// Payer is the debit party's number, or "" when it is absent or masked.
func (r StatusResult) Payer() string {
	n, _, _ := strings.Cut(r.DebitParty, " ")
	if msisdnRE.MatchString(n) {
		return n
	}
	return ""
}

// ParseStatusResult reads the body Daraja posts to the Transaction Status result (or timeout) URL. A completed result
// must name the receipt and a positive whole amount.
func ParseStatusResult(body []byte) (StatusResult, error) {
	var env struct {
		Result struct {
			ResultCode               json.RawMessage `json:"ResultCode"`
			ResultDesc               string          `json:"ResultDesc"`
			OriginatorConversationID string          `json:"OriginatorConversationID"`
			ConversationID           string          `json:"ConversationID"`
			Parameters               *struct {
				Items json.RawMessage `json:"ResultParameter"`
			} `json:"ResultParameters"`
			Reference *struct {
				Item json.RawMessage `json:"ReferenceItem"`
			} `json:"ReferenceData"`
		} `json:"Result"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&env); err != nil {
		return StatusResult{}, fmt.Errorf("%w: not json", ErrBadCallback)
	}
	in := env.Result
	code, ok := resultCode(in.ResultCode)
	if !ok || in.OriginatorConversationID == "" {
		return StatusResult{}, fmt.Errorf("%w: missing result code or conversation id", ErrBadCallback)
	}
	r := StatusResult{ResultCode: code, ResultDesc: in.ResultDesc, OriginatorConversationID: in.OriginatorConversationID, ConversationID: in.ConversationID}
	if in.Reference != nil {
		for _, it := range keyValues(in.Reference.Item) {
			if it.Key == "Occasion" {
				r.Occasion = strings.Trim(string(it.Value), `"`)
			}
		}
	}
	if in.Parameters != nil {
		for _, it := range keyValues(in.Parameters.Items) {
			str := strings.Trim(string(it.Value), `"`)
			switch it.Key {
			case "ReceiptNo":
				r.Receipt = str
			case "Amount":
				if a, ok := wholeKES(it.Value); ok {
					r.AmountKES = a
				} else if code == 0 {
					return StatusResult{}, fmt.Errorf("%w: amount is not a positive whole number of shillings", ErrBadCallback)
				}
			case "TransactionStatus":
				r.Status = str
			case "CreditPartyName":
				r.CreditParty = str
			case "DebitPartyName":
				r.DebitParty = str
			}
		}
	}
	if r.Completed() && (r.Receipt == "" || r.AmountKES == 0) {
		return StatusResult{}, fmt.Errorf("%w: completed without receipt or amount", ErrBadCallback)
	}
	return r, nil
}

type keyValue struct {
	Key   string          `json:"Key"`
	Value json.RawMessage `json:"Value"`
}

// keyValues reads Daraja's key/value parameters, sent as a list or, when there is one, as a single object.
func keyValues(raw json.RawMessage) []keyValue {
	var items []keyValue
	if json.Unmarshal(raw, &items) == nil {
		return items
	}
	var one keyValue
	if json.Unmarshal(raw, &one) == nil && one.Key != "" {
		return []keyValue{one}
	}
	return nil
}
