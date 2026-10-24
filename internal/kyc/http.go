package kyc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

const providerTimeout = 15 * time.Second

// HTTPProvider calls a registry gateway: POST {"id_number","full_name","date_of_birth"} and expects
// {"found","name_match","dob_match","reference"}. Anything that is not a clean 200 JSON answer is an outage (an
// error), never a mismatch, so a failing gateway cannot silently send everyone to manual review.
type HTTPProvider struct {
	url, key string
	hc       *http.Client
}

// NewHTTPProvider returns a Provider for the gateway at url.
func NewHTTPProvider(url, key string, hc *http.Client) *HTTPProvider {
	return &HTTPProvider{url: url, key: key, hc: hc}
}

// Name implements Provider.
func (*HTTPProvider) Name() string { return "http" }

// Verify implements Provider.
func (p *HTTPProvider) Verify(ctx context.Context, r Request) (Result, error) {
	body, err := json.Marshal(map[string]string{
		"id_number": r.IDNumber, "full_name": r.FullName, "date_of_birth": r.DateOfBirth.Format(time.DateOnly),
	})
	if err != nil {
		return Result{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, providerTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, bytes.NewReader(body))
	if err != nil {
		return Result{}, errors.New("kyc: build request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.key)
	resp, err := p.hc.Do(req)
	if err != nil {
		return Result{}, errors.New("kyc: registry gateway unreachable")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Result{}, fmt.Errorf("kyc: registry gateway returned status %d", resp.StatusCode)
	}
	var out struct {
		Found     *bool  `json:"found"`
		NameMatch bool   `json:"name_match"`
		DOBMatch  bool   `json:"dob_match"`
		Reference string `json:"reference"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out); err != nil || out.Found == nil {
		return Result{}, errors.New("kyc: registry gateway returned an unreadable answer")
	}
	return Result{Found: *out.Found, NameMatch: out.NameMatch, DOBMatch: out.DOBMatch, Ref: out.Reference}, nil
}

// UnavailableProvider refuses every verification (no registry configured).
type UnavailableProvider struct{}

// Name implements Provider.
func (UnavailableProvider) Name() string { return "unavailable" }

// Verify implements Provider.
func (UnavailableProvider) Verify(context.Context, Request) (Result, error) {
	return Result{}, ErrProviderUnavailable
}

// ManualProvider means there is no registry: every submission waits for a staff decision (owner decision 2026-10-24,
// ADR 0009: identities are checked by hand instead of a paid registry). Submit recognises it and never runs a job.
type ManualProvider struct{}

// Name implements Provider.
func (ManualProvider) Name() string { return "manual" }

// Verify implements Provider; it is never called for manual submissions.
func (ManualProvider) Verify(context.Context, Request) (Result, error) {
	return Result{}, ErrProviderUnavailable
}
