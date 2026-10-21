// Package notifications sends SMS. The gateway vendor is a business decision, so the adapter speaks a small generic
// contract (POST {"to","message"} with a bearer key) that vendor shims and the local SMS mock can implement.
package notifications

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// ErrSMSUnavailable is returned when no gateway is configured; sending fails closed.
var ErrSMSUnavailable = errors.New("notifications: sms gateway not configured")

const smsTimeout = 10 * time.Second

// HTTPSMS posts messages to a gateway.
type HTTPSMS struct {
	url, key string
	hc       *http.Client
}

// NewHTTPSMS returns a sender for the gateway at url authenticated with key.
func NewHTTPSMS(url, key string, hc *http.Client) *HTTPSMS {
	return &HTTPSMS{url: url, key: key, hc: hc}
}

// Send delivers text to msisdn. Errors carry only the status: the message (which may hold an OTP) and the gateway's
// response body are never included, so they cannot end up in logs.
func (s *HTTPSMS) Send(ctx context.Context, msisdn, text string) error {
	body, err := json.Marshal(map[string]string{"to": msisdn, "message": text})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, smsTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(body))
	if err != nil {
		return errors.New("notifications: build request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.key)
	resp, err := s.hc.Do(req)
	if err != nil {
		return errors.New("notifications: sms gateway unreachable")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("notifications: sms gateway returned status %d", resp.StatusCode)
	}
	return nil
}

// UnavailableSMS refuses every send.
type UnavailableSMS struct{}

// Send implements the sender interface.
func (UnavailableSMS) Send(context.Context, string, string) error { return ErrSMSUnavailable }
