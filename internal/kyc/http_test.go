package kyc_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/itsivali/lottery-platform/internal/kyc"
)

func TestHTTPProvider_MapsTheRegistryAnswer(t *testing.T) {
	var in map[string]string
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&in)
		_ = json.NewEncoder(w).Encode(map[string]any{"found": true, "name_match": true, "dob_match": false, "reference": "ref-9"})
	}))
	defer srv.Close()
	p := kyc.NewHTTPProvider(srv.URL, "key", srv.Client())
	res, err := p.Verify(context.Background(), kyc.Request{IDNumber: "12345678", FullName: "Jane Doe", DateOfBirth: time.Date(1990, 5, 17, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Found || !res.NameMatch || res.DOBMatch || res.Ref != "ref-9" {
		t.Fatalf("result = %+v", res)
	}
	if auth != "Bearer key" || in["id_number"] != "12345678" || in["full_name"] != "Jane Doe" || in["date_of_birth"] != "1990-05-17" {
		t.Fatalf("auth=%q body=%v", auth, in)
	}
	if p.Name() != "http" {
		t.Fatalf("name = %q", p.Name())
	}
}

func TestHTTPProvider_ServerErrorsAndGarbageAreErrorsNotMismatches(t *testing.T) {
	for name, h := range map[string]http.HandlerFunc{
		"500":     func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) },
		"429":     func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(429) },
		"garbage": func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>")) },
		"empty":   func(w http.ResponseWriter, _ *http.Request) {},
	} {
		srv := httptest.NewServer(h)
		_, err := kyc.NewHTTPProvider(srv.URL, "k", srv.Client()).Verify(context.Background(), kyc.Request{IDNumber: "12345678", FullName: "Jane Doe"})
		srv.Close()
		if err == nil {
			t.Errorf("%s: treated as a successful (mismatch) answer instead of an outage", name)
		}
	}
}

func TestUnavailableProvider_FailsClosed(t *testing.T) {
	if _, err := (kyc.UnavailableProvider{}).Verify(context.Background(), kyc.Request{}); !errors.Is(err, kyc.ErrProviderUnavailable) {
		t.Fatalf("err = %v", err)
	}
}
