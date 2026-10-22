// Package daraja is the client for Safaricom's M-Pesa Daraja API: OAuth, STK Push (Lipa na M-Pesa Online), the STK
// status query and STK callback parsing. It knows nothing about payments, wallets or the database.
//
// Errors fall into two classes so callers can decide between retrying and giving up: ErrUnavailable (network trouble,
// 5xx, throttling, unreadable answers: retry later, the outcome is unknown) and ErrRejected (Daraja understood the
// request and refused it: do not retry). Error texts never contain credentials, tokens, phone numbers or bodies.
package daraja

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/itsivali/lottery-platform/internal/platform/clock"
)

// Errors returned by the client.
var (
	ErrUnavailable    = errors.New("daraja: unavailable")
	ErrRejected       = errors.New("daraja: rejected")
	ErrInvalidRequest = errors.New("daraja: invalid request")
	ErrBadCallback    = errors.New("daraja: malformed callback")
)

// MaxSTKAmountKES is the largest single M-Pesa transaction Safaricom allows.
const MaxSTKAmountKES = 250_000

const (
	requestTimeout = 30 * time.Second
	// refresh a token this long before Daraja says it expires, so it never lapses mid-request
	tokenMargin   = time.Minute
	maxBody       = 1 << 16
	timestampFmt  = "20060102150405"
	processingErr = "500.001.1001" // STK query: "The transaction is being processed"
	// stillProcessing is a result code (answered with HTTP 200) that means only "not finished yet"; treating it as a
	// failure would fail a payment the customer may still complete. Seen live from the sandbox.
	stillProcessing = 4999
)

var msisdnRE = regexp.MustCompile(`^254[17][0-9]{8}$`)

// Config is the Daraja app and paybill configuration.
type Config struct {
	BaseURL        string // https://sandbox.safaricom.co.ke, or a local mock
	ConsumerKey    string
	ConsumerSecret string
	ShortCode      string // paybill (Lipa na M-Pesa Online shortcode)
	Passkey        string
	CallbackURL    string // where Daraja posts STK results; Safaricom only calls https
	// AllowProduction must be set to talk to api.safaricom.co.ke. Without it a production base URL is refused, so
	// production credentials cannot be used by accident.
	AllowProduction bool
}

func (c Config) validate() error {
	for name, v := range map[string]string{
		"consumer key": c.ConsumerKey, "consumer secret": c.ConsumerSecret, "shortcode": c.ShortCode,
		"passkey": c.Passkey, "callback url": c.CallbackURL,
	} {
		if v == "" {
			return fmt.Errorf("daraja: %s is required", name)
		}
	}
	base, err := url.Parse(c.BaseURL)
	if err != nil || (base.Scheme != "https" && base.Scheme != "http") || base.Host == "" {
		return errors.New("daraja: base url must be an absolute http(s) url")
	}
	cb, err := url.Parse(c.CallbackURL)
	if err != nil || cb.Host == "" {
		return errors.New("daraja: callback url must be absolute")
	}
	safaricom := strings.HasSuffix(base.Hostname(), "safaricom.co.ke")
	if safaricom && (base.Scheme != "https" || cb.Scheme != "https") {
		return errors.New("daraja: Safaricom is reached, and calls back, over https only")
	}
	if base.Hostname() == "api.safaricom.co.ke" && !c.AllowProduction {
		return errors.New("daraja: production base url refused (set the explicit production switch)")
	}
	return nil
}

// Client calls Daraja. It is safe for concurrent use.
type Client struct {
	cfg Config
	hc  *http.Client
	clk clock.Clock

	mu      sync.Mutex
	token   string
	expires time.Time
}

// New returns a client. clk supplies the request timestamp and token expiry (a wall clock in production).
func New(cfg Config, hc *http.Client, clk clock.Clock) (*Client, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	return &Client{cfg: cfg, hc: hc, clk: clk}, nil
}

func (c *Client) timestamp(ctx context.Context) (string, error) {
	t, err := c.clk.Now(ctx, nil)
	if err != nil {
		return "", err
	}
	return clock.InNairobi(t).Format(timestampFmt), nil
}

// accessToken returns a cached token, fetching a new one when none is cached or it is about to expire.
func (c *Client) accessToken(ctx context.Context, force bool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now, err := c.clk.Now(ctx, nil)
	if err != nil {
		return "", err
	}
	if !force && c.token != "" && now.Before(c.expires.Add(-tokenMargin)) {
		return c.token, nil
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.BaseURL+"/oauth/v1/generate?grant_type=client_credentials", nil)
	if err != nil {
		return "", fmt.Errorf("%w: build token request", ErrUnavailable)
	}
	req.SetBasicAuth(c.cfg.ConsumerKey, c.cfg.ConsumerSecret)
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: token endpoint unreachable", ErrUnavailable)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: token endpoint returned status %d", ErrUnavailable, resp.StatusCode)
	}
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   string `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&out); err != nil || out.AccessToken == "" {
		return "", fmt.Errorf("%w: unreadable token answer", ErrUnavailable)
	}
	secs, err := strconv.Atoi(out.ExpiresIn)
	if err != nil || secs <= 0 {
		return "", fmt.Errorf("%w: unreadable token lifetime", ErrUnavailable)
	}
	c.token, c.expires = out.AccessToken, now.Add(time.Duration(secs)*time.Second)
	return c.token, nil
}

// apiError is Daraja's error envelope.
type apiError struct {
	RequestID    string `json:"requestId"`
	ErrorCode    string `json:"errorCode"`
	ErrorMessage string `json:"errorMessage"`
}

// call posts body to path with a bearer token and returns the status and response body. A 401 (token revoked or
// expired early on Daraja's side) is retried once with a fresh token.
func (c *Client) call(ctx context.Context, path string, body any) (int, []byte, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	for attempt := 0; ; attempt++ {
		tok, err := c.accessToken(ctx, attempt > 0)
		if err != nil {
			return 0, nil, err
		}
		status, raw, err := c.post(ctx, path, tok, payload)
		if err != nil {
			return 0, nil, err
		}
		if status == http.StatusUnauthorized && attempt == 0 {
			continue
		}
		return status, raw, nil
	}
}

func (c *Client) post(ctx context.Context, path, tok string, payload []byte) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+path, bytes.NewReader(payload))
	if err != nil {
		return 0, nil, fmt.Errorf("%w: build request", ErrUnavailable)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: %s unreachable", ErrUnavailable, path)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return 0, nil, fmt.Errorf("%w: reading %s answer", ErrUnavailable, path)
	}
	return resp.StatusCode, raw, nil
}

// classify turns a non-2xx answer into an error: 4xx are rejections (Daraja understood and refused), the rest outages.
func classify(path string, status int, raw []byte) error {
	var e apiError
	_ = json.Unmarshal(raw, &e)
	code := e.ErrorCode
	if code == "" {
		code = "none"
	}
	if status >= 400 && status < 500 && status != http.StatusTooManyRequests {
		return fmt.Errorf("%w: %s status %d code %s", ErrRejected, path, status, code)
	}
	return fmt.Errorf("%w: %s status %d code %s", ErrUnavailable, path, status, code)
}

func (c *Client) password(ts string) string {
	return base64.StdEncoding.EncodeToString([]byte(c.cfg.ShortCode + c.cfg.Passkey + ts))
}

// STKPushRequest asks the customer's phone to approve a paybill payment.
type STKPushRequest struct {
	MSISDN      string // 2547XXXXXXXX / 2541XXXXXXXX: the phone that receives the prompt and pays
	AmountKES   int64
	AccountRef  string // shown to the customer; at most 12 characters
	Description string
}

// STKPushResult identifies an accepted push; the outcome arrives later by callback (or the status query).
type STKPushResult struct {
	MerchantRequestID string
	CheckoutRequestID string
}

// STKPush sends the payment prompt. Acceptance means only that the prompt was queued, never that anyone paid.
func (c *Client) STKPush(ctx context.Context, r STKPushRequest) (STKPushResult, error) {
	if r.AmountKES <= 0 || r.AmountKES > MaxSTKAmountKES || !msisdnRE.MatchString(r.MSISDN) ||
		r.AccountRef == "" || len(r.AccountRef) > 12 || len(r.Description) > 13 {
		return STKPushResult{}, ErrInvalidRequest
	}
	ts, err := c.timestamp(ctx)
	if err != nil {
		return STKPushResult{}, err
	}
	const path = "/mpesa/stkpush/v1/processrequest"
	status, raw, err := c.call(ctx, path, map[string]any{
		"BusinessShortCode": c.cfg.ShortCode, "Password": c.password(ts), "Timestamp": ts,
		"TransactionType": "CustomerPayBillOnline", "Amount": r.AmountKES, "PartyA": r.MSISDN, "PartyB": c.cfg.ShortCode,
		"PhoneNumber": r.MSISDN, "CallBackURL": c.cfg.CallbackURL, "AccountReference": r.AccountRef,
		"TransactionDesc": r.Description,
	})
	if err != nil {
		return STKPushResult{}, err
	}
	if status != http.StatusOK {
		return STKPushResult{}, classify(path, status, raw)
	}
	var out struct {
		MerchantRequestID string `json:"MerchantRequestID"`
		CheckoutRequestID string `json:"CheckoutRequestID"`
		ResponseCode      string `json:"ResponseCode"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return STKPushResult{}, fmt.Errorf("%w: unreadable push answer", ErrUnavailable)
	}
	if out.ResponseCode != "0" && out.ResponseCode != "" {
		return STKPushResult{}, fmt.Errorf("%w: push response code %s", ErrRejected, out.ResponseCode)
	}
	if out.ResponseCode == "" || out.CheckoutRequestID == "" {
		return STKPushResult{}, fmt.Errorf("%w: push answer without a checkout id", ErrUnavailable)
	}
	return STKPushResult{MerchantRequestID: out.MerchantRequestID, CheckoutRequestID: out.CheckoutRequestID}, nil
}

// State is the outcome of an STK request as Daraja reports it.
type State string

// STK outcomes.
const (
	Pending State = "pending" // the customer has not answered yet
	Paid    State = "paid"
	Failed  State = "failed" // cancelled, timed out, insufficient funds, wrong PIN...: no money moved
)

// STKStatus is the answer of the status query.
type STKStatus struct {
	State      State
	ResultCode int
	ResultDesc string
}

// STKQuery asks Daraja for the outcome of a push. It is the authoritative check behind every callback: a callback
// alone is never trusted to move money.
func (c *Client) STKQuery(ctx context.Context, checkoutRequestID string) (STKStatus, error) {
	ts, err := c.timestamp(ctx)
	if err != nil {
		return STKStatus{}, err
	}
	const path = "/mpesa/stkpushquery/v1/query"
	status, raw, err := c.call(ctx, path, map[string]any{
		"BusinessShortCode": c.cfg.ShortCode, "Password": c.password(ts), "Timestamp": ts,
		"CheckoutRequestID": checkoutRequestID,
	})
	if err != nil {
		return STKStatus{}, err
	}
	if status != http.StatusOK {
		var e apiError
		if json.Unmarshal(raw, &e) == nil && e.ErrorCode == processingErr {
			return STKStatus{State: Pending}, nil
		}
		return STKStatus{}, classify(path, status, raw)
	}
	var out struct {
		ResultCode json.RawMessage `json:"ResultCode"`
		ResultDesc string          `json:"ResultDesc"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return STKStatus{}, fmt.Errorf("%w: unreadable query answer", ErrUnavailable)
	}
	code, ok := resultCode(out.ResultCode)
	if !ok {
		return STKStatus{}, fmt.Errorf("%w: query answer without a result code", ErrUnavailable)
	}
	st := STKStatus{State: Failed, ResultCode: code, ResultDesc: out.ResultDesc}
	switch code {
	case 0:
		st.State = Paid
	case stillProcessing:
		st.State = Pending
	}
	return st, nil
}

// resultCode reads a result code Daraja sends either as a number or as a numeric string.
func resultCode(raw json.RawMessage) (int, bool) {
	s := strings.Trim(string(raw), `"`)
	if s == "" || s == "null" {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	return n, err == nil
}
